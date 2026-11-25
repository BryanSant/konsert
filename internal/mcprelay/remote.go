package mcprelay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"github.com/BryanSant/syre/internal/protocol"
)

// chunkSize is the most response body one mcp.body frame carries; the handler's own
// flushes cut chunks earlier.
const chunkSize = 32 << 10

// Remote is the hub's side of the relay for one link.
type Remote struct {
	// Ctx is the link's lifetime: every request ends with it (its values are not passed
	// on to requests).
	Ctx context.Context
	// Handler serves a chat's MCP endpoint. It sees requests for MCPChatPathPrefix +
	// chat id with the path value "id" set; it must authenticate them itself.
	Handler http.Handler
	// Send queues one envelope on the link, waiting while the link's queue is full, until
	// ctx ends. It must encode payload before returning.
	Send func(ctx context.Context, msgType string, payload any) error
	// RemoteAddr labels relayed requests (r.RemoteAddr).
	RemoteAddr string

	mu       sync.Mutex
	inflight map[string]context.CancelFunc
}

// Handle starts or cancels a relayed request. It reports whether env was a relay
// frame. It never blocks.
func (r *Remote) Handle(env protocol.Envelope) bool {
	switch env.Type {
	case protocol.MsgMCPRequest:
		p, err := protocol.DecodePayload[protocol.MCPRequest](env)
		if err == nil {
			r.start(p)
		}
	case protocol.MsgMCPCancel:
		p, err := protocol.DecodePayload[protocol.MCPCancel](env)
		if err == nil {
			r.mu.Lock()
			cancel := r.inflight[p.ID]
			r.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	default:
		return false
	}
	return true
}

// Inflight is the number of requests being served (tests).
func (r *Remote) Inflight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.inflight)
}

func (r *Remote) fail(id, msg string) {
	go func() { _ = r.Send(r.Ctx, protocol.MsgMCPEnd, protocol.MCPEnd{ID: id, Error: msg}) }()
}

func (r *Remote) start(p protocol.MCPRequest) {
	if !validID(p.ID) {
		return // nothing to answer to
	}
	switch {
	case !validID(p.ChatID):
		r.fail(p.ID, "invalid chat id")
		return
	case !allowedMethod(p.Method):
		r.fail(p.ID, "method not allowed")
		return
	case len(p.Body) > protocol.MCPMaxBody:
		r.fail(p.ID, "request body too large")
		return
	}
	// The request ends with the link but carries none of its context's values: the link
	// may have been accepted by the hub's HTTP server, whose values (local address,
	// identity) describe that connection, not this request.
	ctx, cancelReq := context.WithCancel(context.Background())
	stop := context.AfterFunc(r.Ctx, cancelReq)
	cancel := func() { stop(); cancelReq() }
	r.mu.Lock()
	if r.inflight == nil {
		r.inflight = map[string]context.CancelFunc{}
	}
	if _, dup := r.inflight[p.ID]; dup {
		r.mu.Unlock()
		cancel()
		r.fail(p.ID, "duplicate request id")
		return
	}
	r.inflight[p.ID] = cancel
	r.mu.Unlock()
	go r.serve(ctx, cancel, p)
}

func (r *Remote) serve(ctx context.Context, cancel context.CancelFunc, p protocol.MCPRequest) {
	defer func() {
		r.mu.Lock()
		delete(r.inflight, p.ID)
		r.mu.Unlock()
		cancel()
	}()
	req, err := http.NewRequestWithContext(ctx, p.Method,
		"http://syred"+protocol.MCPChatPathPrefix+url.PathEscape(p.ChatID), bytes.NewReader(p.Body))
	if err != nil {
		_ = r.Send(ctx, protocol.MsgMCPEnd, protocol.MCPEnd{ID: p.ID, Error: "bad request"})
		return
	}
	req.Header = requestHeader(p.Header)
	req.RemoteAddr = r.RemoteAddr
	req.SetPathValue("id", p.ChatID)
	rw := &relayWriter{ctx: ctx, id: p.ID, send: r.Send, header: http.Header{}}
	failed := ""
	func() {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					failed = "aborted"
				} else {
					failed = fmt.Sprint("handler panic: ", v)
				}
			}
		}()
		r.Handler.ServeHTTP(rw, req)
	}()
	rw.finish(failed)
}

// relayWriter is the http.ResponseWriter of a relayed request: the head goes out with
// the first flush (or the end), body bytes in chunks on every Flush.
type relayWriter struct {
	ctx  context.Context
	id   string
	send func(ctx context.Context, msgType string, payload any) error

	mu       sync.Mutex
	header   http.Header
	status   int
	headSent bool
	buf      []byte
	err      error
}

func (w *relayWriter) Header() http.Header { return w.header }

func (w *relayWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 && code >= 200 {
		w.status = code
	}
}

func (w *relayWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.buf = append(w.buf, b...)
	if len(w.buf) >= chunkSize {
		w.flushLocked()
	}
	if w.err != nil {
		return 0, w.err
	}
	return len(b), nil
}

// Flush sends the head (if not yet sent) and the buffered body.
func (w *relayWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushLocked()
}

// errLinkGone is returned by writes after the link (or the request) ended.
var errLinkGone = errors.New("mcp relay: the link to syred ended")

func (w *relayWriter) flushLocked() {
	if w.err != nil {
		return
	}
	if !w.headSent {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		if err := w.send(w.ctx, protocol.MsgMCPHead, protocol.MCPHead{ID: w.id, Status: w.status, Header: w.header.Clone()}); err != nil {
			w.err = errLinkGone
			return
		}
		w.headSent = true
	}
	for len(w.buf) > 0 {
		n := min(len(w.buf), chunkSize)
		if err := w.send(w.ctx, protocol.MsgMCPBody, protocol.MCPBody{ID: w.id, Data: w.buf[:n]}); err != nil {
			w.err = errLinkGone
			return
		}
		w.buf = w.buf[n:]
	}
	w.buf = nil
}

func (w *relayWriter) finish(failed string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if failed == "" {
		w.flushLocked()
	}
	if w.err != nil {
		return // the link is gone: syred fails the request itself
	}
	_ = w.send(w.ctx, protocol.MsgMCPEnd, protocol.MCPEnd{ID: w.id, Error: failed})
}
