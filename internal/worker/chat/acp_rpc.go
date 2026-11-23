package chat

// Minimal line-delimited JSON-RPC 2.0 peer used by the ACP engine. Each message is one
// JSON object per line. Requests from the agent (session/request_permission, fs/*,
// terminal/*) and notifications (session/update) go to the handler; responses complete
// pending calls.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// rpcError is a JSON-RPC error object returned by the agent.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	msg := e.Message
	if d := e.dataText(); d != "" && d != msg {
		msg += ": " + d
	}
	return fmt.Sprintf("%s (code %d)", msg, e.Code)
}

// dataText renders error data: a string, a {message} object, or the raw JSON.
func (e *rpcError) dataText() string {
	if len(e.Data) == 0 || string(e.Data) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(e.Data, &s) == nil {
		return s
	}
	var o struct {
		Message string `json:"message"`
		Details string `json:"details"`
	}
	if json.Unmarshal(e.Data, &o) == nil && (o.Message != "" || o.Details != "") {
		if o.Message == "" {
			return o.Details
		}
		return o.Message
	}
	return string(e.Data)
}

// JSON-RPC error codes.
const (
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
	acpAuthRequired   = -32000
	acpNotFound       = -32002
)

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

// errRPCClosed means the agent's stdout closed (it exited) while a call was pending.
var errRPCClosed = errors.New("agent connection closed")

// rpcHandler receives agent-initiated messages. id is nil for notifications. It runs on
// the read loop and must not block.
type rpcHandler func(method string, id json.RawMessage, params json.RawMessage)

type rpcConn struct {
	w       io.Writer
	wmu     sync.Mutex
	handler rpcHandler

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan rpcResult
	closed  bool
	done    chan struct{}

	// badLines counts non-JSON stdout lines (diagnostics only); lastBad keeps the last one.
	badLines int
	lastBad  string
}

func newRPCConn(r io.Reader, w io.Writer, h rpcHandler) *rpcConn {
	c := &rpcConn{w: w, handler: h, pending: map[string]chan rpcResult{}, done: make(chan struct{})}
	go c.readLoop(r)
	return c
}

// Done is closed when the read side ends (agent exited or closed stdout).
func (c *rpcConn) Done() <-chan struct{} { return c.done }

func (c *rpcConn) readLoop(r io.Reader) {
	defer c.shutdown()
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			c.dispatch(line)
		}
		if err != nil {
			return
		}
	}
}

func (c *rpcConn) dispatch(line []byte) {
	var m rpcMessage
	if err := json.Unmarshal(line, &m); err != nil || (m.Method == "" && len(m.ID) == 0) {
		c.mu.Lock()
		c.badLines++
		c.lastBad = truncate(string(bytes.TrimSpace(line)), 300)
		c.mu.Unlock()
		return
	}
	if m.Method != "" {
		if c.handler != nil {
			c.handler(m.Method, m.ID, m.Params)
		}
		return
	}
	key := idKey(m.ID)
	c.mu.Lock()
	ch := c.pending[key]
	delete(c.pending, key)
	c.mu.Unlock()
	if ch == nil {
		return
	}
	if m.Error != nil {
		ch <- rpcResult{err: m.Error}
		return
	}
	ch <- rpcResult{result: m.Result}
}

func (c *rpcConn) shutdown() {
	c.mu.Lock()
	c.closed = true
	pend := c.pending
	c.pending = map[string]chan rpcResult{}
	c.mu.Unlock()
	for _, ch := range pend {
		ch <- rpcResult{err: errRPCClosed}
	}
	close(c.done)
}

func idKey(id json.RawMessage) string {
	var n json.Number
	if json.Unmarshal(id, &n) == nil {
		return n.String()
	}
	var s string
	if json.Unmarshal(id, &s) == nil {
		return "s:" + s
	}
	return string(id)
}

func (c *rpcConn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(b)
	return err
}

// Call sends a request and waits for its response (or ctx / connection end).
func (c *rpcConn) Call(ctx context.Context, method string, params any, result any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errRPCClosed
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResult, 1)
	c.pending[strconv.FormatInt(id, 10)] = ch
	c.mu.Unlock()

	if params == nil {
		params = struct{}{}
	}
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, strconv.FormatInt(id, 10))
		c.mu.Unlock()
		return fmt.Errorf("write %s: %w", method, err)
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if result != nil && len(r.result) > 0 && string(r.result) != "null" {
			if err := json.Unmarshal(r.result, result); err != nil {
				return &protocolError{msg: fmt.Sprintf("decode %s result: %v", method, err)}
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, strconv.FormatInt(id, 10))
		c.mu.Unlock()
		return ctx.Err()
	}
}

// Notify sends a notification (no response expected).
func (c *rpcConn) Notify(method string, params any) error {
	if params == nil {
		params = struct{}{}
	}
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// Reply answers an agent request.
func (c *rpcConn) Reply(id json.RawMessage, result any) error {
	if result == nil {
		result = struct{}{}
	}
	return c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// ReplyError answers an agent request with an error.
func (c *rpcConn) ReplyError(id json.RawMessage, code int, msg string) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

// protocolError marks malformed agent output.
type protocolError struct{ msg string }

func (e *protocolError) Error() string { return e.msg }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary.
	for n > 0 && n < len(s) && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n] + "…"
}
