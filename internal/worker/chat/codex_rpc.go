package chat

// Minimal JSON-RPC client for `codex app-server` (newline-delimited JSON over stdio; the
// server omits the "jsonrpc" member and so do we).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
)

type codexRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *codexRPCError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

type codexWire struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *codexRPCError  `json:"error,omitempty"`
}

type codexReply struct {
	result json.RawMessage
	err    error
}

type codexConn struct {
	proc  ccProcess
	trace func(dir string, line []byte)

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[string]chan codexReply
	closed  bool

	onNotify  func(method string, params json.RawMessage)
	onRequest func(id json.RawMessage, method string, params json.RawMessage)
	onActive  func() // any line received
}

func newCodexConn(proc ccProcess) *codexConn {
	return &codexConn{proc: proc, pending: map[string]chan codexReply{}}
}

// serve reads until EOF; it returns when the process closes stdout.
func (c *codexConn) serve() {
	sc := ccLineReader(c.proc.Stdout())
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if c.trace != nil {
			c.trace("<", line)
		}
		var w codexWire
		if json.Unmarshal(line, &w) != nil {
			continue
		}
		if c.onActive != nil {
			c.onActive()
		}
		switch {
		case w.Method != "" && len(w.ID) > 0:
			if c.onRequest != nil {
				c.onRequest(w.ID, w.Method, w.Params)
			} else {
				_ = c.respondError(w.ID, -32601, "unsupported")
			}
		case w.Method != "":
			if c.onNotify != nil {
				c.onNotify(w.Method, w.Params)
			}
		case len(w.ID) > 0:
			key := codexIDKey(w.ID)
			c.mu.Lock()
			ch := c.pending[key]
			delete(c.pending, key)
			c.mu.Unlock()
			if ch != nil {
				if w.Error != nil {
					ch <- codexReply{err: w.Error}
				} else {
					ch <- codexReply{result: w.Result}
				}
			}
		}
	}
	c.mu.Lock()
	c.closed = true
	for k, ch := range c.pending {
		ch <- codexReply{err: errors.New("codex app-server exited")}
		delete(c.pending, k)
	}
	c.mu.Unlock()
}

func codexIDKey(id json.RawMessage) string {
	var n json.Number
	if json.Unmarshal(id, &n) == nil {
		return n.String()
	}
	var s string
	if json.Unmarshal(id, &s) == nil {
		return s
	}
	return string(id)
}

func (c *codexConn) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if c.trace != nil {
		c.trace(">", data)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.proc.Stdin().Write(append(data, '\n'))
	return err
}

// call sends a request and waits for its result.
func (c *codexConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("codex app-server exited")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan codexReply, 1)
	c.pending[strconv.FormatInt(id, 10)] = ch
	c.mu.Unlock()
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := c.write(msg); err != nil {
		c.mu.Lock()
		delete(c.pending, strconv.FormatInt(id, 10))
		c.mu.Unlock()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, strconv.FormatInt(id, 10))
		c.mu.Unlock()
		return nil, fmt.Errorf("codex did not answer %s: %w", method, ctx.Err())
	}
}

func (c *codexConn) notify(method string, params any) error {
	msg := map[string]any{"method": method}
	if params != nil {
		msg["params"] = params
	}
	return c.write(msg)
}

func (c *codexConn) respond(id json.RawMessage, result any) error {
	return c.write(map[string]any{"id": id, "result": result})
}

func (c *codexConn) respondError(id json.RawMessage, code int, msg string) error {
	return c.write(map[string]any{"id": id, "error": map[string]any{"code": code, "message": msg}})
}

// codexHandshake runs initialize + initialized.
func codexHandshake(ctx context.Context, c *codexConn) error {
	_, err := c.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "konsert", "title": "Konsert", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": false, "requestAttestation": false},
	})
	if err != nil {
		return err
	}
	return c.notify("initialized", nil)
}
