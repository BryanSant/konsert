package mcprelay

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// pipe is an in-memory hub link: frames are encoded and decoded as on the wire, and
// each direction is delivered in order by one goroutine (like a WebSocket read loop).
type pipe struct {
	local  *Local
	remote *Remote
	link   *Link
	cancel context.CancelFunc
	toHub  chan []byte
	toD    chan []byte
	done   chan struct{}

	dropOnce sync.Once
}

func newPipe(t *testing.T, local *Local, h http.Handler) *pipe {
	t.Helper()
	// Like a link accepted by the hub's loopback HTTP server: its values must not reach
	// relayed requests (the MCP handler would take them for a DNS-rebinding attempt).
	base := context.WithValue(context.Background(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8420})
	ctx, cancel := context.WithCancel(base)
	p := &pipe{local: local, cancel: cancel, toHub: make(chan []byte, 64), toD: make(chan []byte, 64), done: make(chan struct{})}
	p.remote = &Remote{
		Ctx:        ctx,
		Handler:    h,
		RemoteAddr: "konsertd:test",
		Send: func(sctx context.Context, msgType string, payload any) error {
			raw, err := protocol.NewEnvelope(msgType, "", payload)
			if err != nil {
				return err
			}
			select {
			case p.toD <- raw:
				return nil
			case <-sctx.Done():
				return sctx.Err()
			case <-p.done:
				return context.Canceled
			}
		},
	}
	p.link = &Link{
		Send: func(msgType string, payload any) bool {
			raw, err := protocol.NewEnvelope(msgType, "", payload)
			if err != nil {
				return false
			}
			select {
			case p.toHub <- raw:
				return true
			case <-p.done:
				return false
			}
		},
		Done: p.done,
	}
	go p.pump(p.toHub, func(env protocol.Envelope) {
		if !p.remote.Handle(env) {
			t.Errorf("hub got a non-relay frame %s", env.Type)
		}
	})
	go p.pump(p.toD, func(env protocol.Envelope) {
		if !local.Deliver(env) {
			t.Errorf("konsertd got a non-relay frame %s", env.Type)
		}
	})
	local.SetLink(p.link)
	t.Cleanup(p.drop)
	return p
}

func (p *pipe) pump(ch chan []byte, deliver func(protocol.Envelope)) {
	for {
		select {
		case raw := <-ch:
			env, err := protocol.DecodeEnvelope(raw)
			if err != nil {
				panic(err)
			}
			deliver(env)
		case <-p.done:
			return
		}
	}
}

// drop ends the link on both sides, as a dead WebSocket does.
func (p *pipe) drop() {
	p.dropOnce.Do(func() {
		close(p.done)
		p.cancel()
	})
}

func startLocal(t *testing.T) (*Local, string) {
	t.Helper()
	l := NewLocal()
	base, err := l.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, base
}

func post(t *testing.T, url, body string, hdr map[string]string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

func TestRelayRoundTrip(t *testing.T) {
	l, base := startLocal(t)
	var got struct {
		path, chat, auth, cookie, body string
	}
	newPipe(t, l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.path, got.chat, got.auth, got.cookie, got.body = r.URL.Path, r.PathValue("id"), r.Header.Get("Authorization"), r.Header.Get("Cookie"), string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "s1")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	resp, err := post(t, base+"/mcp/chat/c1", `{"jsonrpc":"2.0"}`, map[string]string{
		"Authorization": "Bearer tok", "Cookie": "konsert_session=x", "Content-Type": "application/json",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted || string(b) != `{"ok":true}` || resp.Header.Get("Mcp-Session-Id") != "s1" ||
		resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("response: %d %q %v", resp.StatusCode, b, resp.Header)
	}
	if got.path != "/mcp/chat/c1" || got.chat != "c1" || got.auth != "Bearer tok" || got.cookie != "" || got.body != `{"jsonrpc":"2.0"}` {
		t.Fatalf("hub saw %+v", got)
	}
	if n := l.Inflight(); n != 0 {
		t.Fatalf("%d requests left in flight", n)
	}
}

func TestRelayRejectsOtherPathsAndMethods(t *testing.T) {
	l, base := startLocal(t)
	newPipe(t, l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected relay of %s %s", r.Method, r.URL.Path)
	}))
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/api/tasks", http.StatusNotFound},
		{http.MethodPost, "/mcp", http.StatusNotFound},
		{http.MethodPost, "/mcp/chat/a/b", http.StatusNotFound},
		{http.MethodPut, "/mcp/chat/c1", http.StatusMethodNotAllowed},
	} {
		req, _ := http.NewRequest(tc.method, base+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
	}
}

func TestRelayStreamsFlushedChunks(t *testing.T) {
	l, base := startLocal(t)
	release := make(chan struct{})
	newPipe(t, l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "data: two\n\n")
	}))
	resp, err := post(t, base+"/mcp/chat/c1", "{}", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || line != "data: one\n" {
		t.Fatalf("first event before release: %q %v", line, err)
	}
	close(release)
	rest, _ := io.ReadAll(br)
	if string(rest) != "\ndata: two\n\n" {
		t.Fatalf("rest %q", rest)
	}
}

func TestRelayConcurrentRequests(t *testing.T) {
	l, base := startLocal(t)
	newPipe(t, l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		// Answer in a scrambled order, in several chunks.
		time.Sleep(time.Duration(len(b)%7) * time.Millisecond)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "%s:%s:%d;", r.PathValue("id"), b, i)
			w.(http.Flusher).Flush()
		}
	}))
	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chat := fmt.Sprintf("c%d", i%4)
			body := fmt.Sprintf("req-%d", i)
			resp, err := post(t, base+"/mcp/chat/"+chat, body, nil)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			want := fmt.Sprintf("%[1]s:%[2]s:0;%[1]s:%[2]s:1;%[1]s:%[2]s:2;", chat, body)
			if string(b) != want {
				errs <- fmt.Errorf("request %d got %q, want %q", i, b, want)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestRelayLargeBodyIsChunked(t *testing.T) {
	l, base := startLocal(t)
	big := strings.Repeat("x", 3*chunkSize+17)
	newPipe(t, l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, big)
	}))
	resp, err := post(t, base+"/mcp/chat/c1", "{}", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != big {
		t.Fatalf("got %d bytes, want %d", len(b), len(big))
	}
}

func TestRelayLinkDropMidRequest(t *testing.T) {
	l, base := startLocal(t)
	started := make(chan struct{}, 2)
	handlerCtxDone := make(chan struct{}, 2)
	block := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("x") != "" {
			t.Error("query string relayed")
		}
		if b, _ := io.ReadAll(r.Body); string(b) == "head-first" {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "partial")
			w.(http.Flusher).Flush()
		}
		started <- struct{}{}
		<-r.Context().Done()
		handlerCtxDone <- struct{}{}
	}
	p := newPipe(t, l, http.HandlerFunc(block))

	// Head not sent yet: a clean 503 the agent can retry.
	type result struct {
		resp *http.Response
		err  error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := post(t, base+"/mcp/chat/c1", "{}", nil)
		res <- result{resp, err}
	}()
	<-started
	// Head already sent: the response is cut, so the agent sees an error.
	res2 := make(chan error, 1)
	gotHead := make(chan struct{})
	go func() {
		resp, err := post(t, base+"/mcp/chat/c1", "head-first", nil)
		close(gotHead)
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		res2 <- err
	}()
	<-started
	<-gotHead
	p.drop()
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	b, _ := io.ReadAll(r.resp.Body)
	r.resp.Body.Close()
	if r.resp.StatusCode != http.StatusServiceUnavailable || r.resp.Header.Get("Retry-After") == "" || !strings.Contains(string(b), "retry") {
		t.Fatalf("drop before head: %d %q", r.resp.StatusCode, b)
	}
	if err := <-res2; err == nil {
		t.Fatal("drop after head: the response ended cleanly")
	}
	for range 2 {
		select {
		case <-handlerCtxDone:
		case <-time.After(5 * time.Second):
			t.Fatal("hub handler not canceled when the link dropped")
		}
	}

	// The agent retries; the request waits for the next link and succeeds on it.
	retry := make(chan result, 1)
	go func() {
		resp, err := post(t, base+"/mcp/chat/c1", "{}", nil)
		retry <- result{resp, err}
	}()
	time.Sleep(100 * time.Millisecond)
	newPipe(t, l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "back") }))
	r = <-retry
	if r.err != nil {
		t.Fatal(r.err)
	}
	b, _ = io.ReadAll(r.resp.Body)
	r.resp.Body.Close()
	if r.resp.StatusCode != http.StatusOK || string(b) != "back" {
		t.Fatalf("retry after reconnect: %d %q", r.resp.StatusCode, b)
	}
}

func TestRelayNoLinkTimesOut(t *testing.T) {
	l, base := startLocal(t)
	l.WaitLink = 100 * time.Millisecond
	resp, err := post(t, base+"/mcp/chat/c1", "{}", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestRelayClientCancelCancelsHub(t *testing.T) {
	l, base := startLocal(t)
	started := make(chan struct{})
	canceled := make(chan struct{})
	p := newPipe(t, l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/mcp/chat/c1", strings.NewReader("{}"))
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	cancel()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("hub handler still running after the agent went away")
	}
	deadline := time.Now().Add(5 * time.Second)
	for p.remote.Inflight() != 0 || l.Inflight() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("in flight: hub %d, konsertd %d", p.remote.Inflight(), l.Inflight())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRelayRemoteRejectsBadRequests(t *testing.T) {
	sent := make(chan protocol.MCPEnd, 4)
	r := &Remote{
		Ctx:     context.Background(),
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("handler reached") }),
		Send: func(_ context.Context, msgType string, payload any) error {
			if e, ok := payload.(protocol.MCPEnd); ok && msgType == protocol.MsgMCPEnd {
				sent <- e
			}
			return nil
		},
	}
	for _, p := range []protocol.MCPRequest{
		{ID: "a", ChatID: "../x", Method: http.MethodPost},
		{ID: "b", ChatID: "c1", Method: http.MethodPut},
		{ID: "c", ChatID: "c1", Method: http.MethodPost, Body: make([]byte, protocol.MCPMaxBody+1)},
	} {
		raw, _ := protocol.NewEnvelope(protocol.MsgMCPRequest, "", p)
		env, _ := protocol.DecodeEnvelope(raw)
		r.Handle(env)
		select {
		case e := <-sent:
			if e.ID != p.ID || e.Error == "" {
				t.Errorf("request %s: end %+v", p.ID, e)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("request %s not rejected", p.ID)
		}
	}
}

// TestRelayMCPSession runs a real MCP Streamable HTTP server behind the relay and a
// real MCP client in front of it (SSE responses included).
func TestRelayMCPSession(t *testing.T) {
	l, base := startLocal(t)
	srv := mcp.NewServer(&mcp.Implementation{Name: "konsert", Version: "test"}, nil)
	type echoIn struct {
		Text string `json:"text"`
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + in.Text}}}, nil, nil
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	newPipe(t, l, h)

	c := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil)
	sess, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp/chat/c1", MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "echo" {
		t.Fatalf("list tools: %+v %v", tools, err)
	}
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); !ok || tc.Text != "echo: hi" {
		t.Fatalf("call: %+v", res.Content)
	}
}
