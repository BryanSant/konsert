package mcprelay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// DefaultWaitLink is how long a request waits for a hub link while konsertd has none
// (a dropped link usually comes back within seconds).
const DefaultWaitLink = 20 * time.Second

// Local is konsertd's side of the relay: the loopback endpoint agents use. The zero
// value is not usable; call NewLocal.
type Local struct {
	// WaitLink overrides DefaultWaitLink.
	WaitLink time.Duration

	mu   sync.Mutex
	link *Link
	up   chan struct{} // closed while link is set
	reqs map[string]*localReq

	startOnce sync.Once
	base      string
	startErr  error
	srv       *http.Server
}

// NewLocal returns a relay with no link and no listener.
func NewLocal() *Local {
	return &Local{up: make(chan struct{}), reqs: map[string]*localReq{}}
}

// Start listens on 127.0.0.1 (an ephemeral port) once and returns the base URL
// (http://127.0.0.1:<port>); later calls return the same.
func (l *Local) Start() (string, error) {
	l.startOnce.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			l.startErr = err
			return
		}
		l.base = "http://" + ln.Addr().String()
		l.mu.Lock()
		l.srv = &http.Server{Handler: l, ReadHeaderTimeout: 10 * time.Second}
		srv := l.srv
		l.mu.Unlock()
		go func() { _ = srv.Serve(ln) }()
	})
	return l.base, l.startErr
}

// Close stops the listener, if it was started.
func (l *Local) Close() error {
	l.mu.Lock()
	srv := l.srv
	l.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Close()
}

// SetLink makes link the current hub link. Requests in flight on an earlier link are
// failed when that link ends; requests waiting for a link use this one.
func (l *Local) SetLink(link *Link) {
	l.mu.Lock()
	if l.link == nil {
		close(l.up)
	}
	l.link = link
	l.mu.Unlock()
	go func() {
		<-link.Done
		l.mu.Lock()
		if l.link == link {
			l.link = nil
			l.up = make(chan struct{})
		}
		var lost []*localReq
		for _, q := range l.reqs {
			if q.link == link {
				lost = append(lost, q)
			}
		}
		l.mu.Unlock()
		for _, q := range lost {
			q.push(frame{kind: frameLost})
		}
	}()
}

// Deliver routes a response frame from the hub to its request. It reports whether env
// was a relay frame. It never blocks.
func (l *Local) Deliver(env protocol.Envelope) bool {
	var id string
	var f frame
	switch env.Type {
	case protocol.MsgMCPHead:
		p, err := protocol.DecodePayload[protocol.MCPHead](env)
		if err != nil {
			return true
		}
		id, f = p.ID, frame{kind: frameHead, status: p.Status, header: p.Header}
	case protocol.MsgMCPBody:
		p, err := protocol.DecodePayload[protocol.MCPBody](env)
		if err != nil {
			return true
		}
		id, f = p.ID, frame{kind: frameBody, data: p.Data}
	case protocol.MsgMCPEnd:
		p, err := protocol.DecodePayload[protocol.MCPEnd](env)
		if err != nil {
			return true
		}
		id, f = p.ID, frame{kind: frameEnd, err: p.Error}
	default:
		return false
	}
	l.mu.Lock()
	q := l.reqs[id]
	l.mu.Unlock()
	if q != nil {
		q.push(f)
	}
	return true
}

// Inflight is the number of requests waiting for the hub (tests).
func (l *Local) Inflight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.reqs)
}

func (l *Local) waitLink(ctx context.Context) *Link {
	wait := l.WaitLink
	if wait <= 0 {
		wait = DefaultWaitLink
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		l.mu.Lock()
		link, up := l.link, l.up
		l.mu.Unlock()
		if link != nil {
			select {
			case <-link.Done:
				// Ended but not yet cleared by SetLink's watcher: look again shortly.
				up = nil
			default:
				return link
			}
		}
		retry := time.After(50 * time.Millisecond)
		if up != nil {
			retry = nil
		}
		select {
		case <-up:
		case <-retry:
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		}
	}
}

// ServeHTTP relays one agent request for MCPChatPathPrefix + chat id to the hub.
func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	chatID, ok := strings.CutPrefix(r.URL.Path, protocol.MCPChatPathPrefix)
	if !ok || !validID(chatID) {
		http.NotFound(w, r)
		return
	}
	if !allowedMethod(r.Method) {
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, protocol.MCPMaxBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read request: "+err.Error(), http.StatusBadRequest)
		return
	}
	link := l.waitLink(r.Context())
	if link == nil {
		unavailable(w, "konsertd is not linked to the hub; retry")
		return
	}
	id := newID()
	q := &localReq{link: link, sig: make(chan struct{}, 1)}
	l.mu.Lock()
	l.reqs[id] = q
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.reqs, id)
		l.mu.Unlock()
	}()
	// A link that ended between waitLink and registering would never fail q.
	select {
	case <-link.Done:
		unavailable(w, "the hub link dropped; retry")
		return
	default:
	}
	if !link.Send(protocol.MsgMCPRequest, protocol.MCPRequest{
		ID: id, ChatID: chatID, Method: r.Method, Header: requestHeader(r.Header), Body: body,
	}) {
		unavailable(w, "the hub link dropped; retry")
		return
	}
	flusher, _ := w.(http.Flusher)
	headSent := false
	for {
		select {
		case <-r.Context().Done():
			link.Send(protocol.MsgMCPCancel, protocol.MCPCancel{ID: id})
			return
		case <-q.sig:
		}
		for _, f := range q.take() {
			switch f.kind {
			case frameHead:
				if headSent {
					continue
				}
				copyResponseHeader(w.Header(), f.header)
				status := f.status
				if status < 200 || status > 999 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				headSent = true
			case frameBody:
				headSent = true
				if _, err := w.Write(f.data); err != nil {
					link.Send(protocol.MsgMCPCancel, protocol.MCPCancel{ID: id})
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			case frameEnd:
				if f.err != "" {
					if !headSent {
						http.Error(w, "hub: "+f.err, http.StatusBadGateway)
						return
					}
					// Cut the response so the agent sees a failure, not a short body.
					panic(http.ErrAbortHandler)
				}
				if !headSent {
					w.WriteHeader(http.StatusOK)
				}
				return
			case frameLost:
				if !headSent {
					unavailable(w, "the hub link dropped; retry")
					return
				}
				panic(http.ErrAbortHandler)
			}
		}
	}
}

func unavailable(w http.ResponseWriter, msg string) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, msg, http.StatusServiceUnavailable)
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type frameKind int

const (
	frameHead frameKind = iota + 1
	frameBody
	frameEnd
	frameLost // the link carrying the request ended
)

type frame struct {
	kind   frameKind
	status int
	header http.Header
	data   []byte
	err    string
}

// localReq queues one request's response frames (unbounded, so the link's read loop
// never waits for a slow agent).
type localReq struct {
	link *Link
	mu   sync.Mutex
	q    []frame
	sig  chan struct{} // capacity 1: "q is non-empty"
}

func (q *localReq) push(f frame) {
	q.mu.Lock()
	q.q = append(q.q, f)
	q.mu.Unlock()
	select {
	case q.sig <- struct{}{}:
	default:
	}
}

func (q *localReq) take() []frame {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.q
	q.q = nil
	return out
}
