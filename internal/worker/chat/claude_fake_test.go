package chat

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// ccFakeProc is an in-memory engine process: a handler sees every JSON line written to
// stdin and may write lines to stdout or exit.
type ccFakeProc struct {
	spec    ccSpec
	inR     *io.PipeReader
	inW     *io.PipeWriter
	outR    *io.PipeReader
	outW    *io.PipeWriter
	outMu   sync.Mutex
	exitCh  chan struct{}
	once    sync.Once
	code    int
	stderr  string
	written []map[string]any
	wmu     sync.Mutex
}

func newCCFakeProc(spec ccSpec, handler func(p *ccFakeProc, msg map[string]any)) *ccFakeProc {
	p := &ccFakeProc{spec: spec, exitCh: make(chan struct{})}
	p.inR, p.inW = io.Pipe()
	p.outR, p.outW = io.Pipe()
	go func() {
		sc := bufio.NewScanner(p.inR)
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			p.wmu.Lock()
			p.written = append(p.written, m)
			p.wmu.Unlock()
			if handler != nil {
				handler(p, m)
			}
		}
		p.exit(0, "")
	}()
	return p
}

func (p *ccFakeProc) Stdin() io.WriteCloser { return p.inW }
func (p *ccFakeProc) Stdout() io.Reader     { return p.outR }

func (p *ccFakeProc) Wait() (int, string) {
	<-p.exitCh
	return p.code, p.stderr
}

func (p *ccFakeProc) Kill() { p.exit(-1, "") }

// exit ends the process with a code and stderr.
func (p *ccFakeProc) exit(code int, stderr string) {
	p.once.Do(func() {
		p.code, p.stderr = code, stderr
		p.outMu.Lock()
		_ = p.outW.Close()
		p.outMu.Unlock()
		_ = p.inR.Close()
		close(p.exitCh)
	})
}

// send writes one stdout line (a JSON value or a raw string).
func (p *ccFakeProc) send(v any) {
	var b []byte
	switch x := v.(type) {
	case string:
		b = []byte(x)
	case []byte:
		b = x
	default:
		b, _ = json.Marshal(v)
	}
	p.outMu.Lock()
	defer p.outMu.Unlock()
	_, _ = p.outW.Write(append(b, '\n'))
}

func (p *ccFakeProc) writtenMsgs() []map[string]any {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	return append([]map[string]any(nil), p.written...)
}

// ccFakeLauncher records launches and builds fake processes with handler.
type ccFakeLauncher struct {
	mu      sync.Mutex
	procs   []*ccFakeProc
	handler func(p *ccFakeProc, msg map[string]any)
}

func (l *ccFakeLauncher) launch(_ context.Context, spec ccSpec) (ccProcess, error) {
	p := newCCFakeProc(spec, l.handler)
	l.mu.Lock()
	l.procs = append(l.procs, p)
	l.mu.Unlock()
	return p, nil
}

func (l *ccFakeLauncher) last() *ccFakeProc {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.procs) == 0 {
		return nil
	}
	return l.procs[len(l.procs)-1]
}

func (l *ccFakeLauncher) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.procs)
}

// ccRecorder collects emitted events.
type ccRecorder struct {
	mu  sync.Mutex
	evs []protocol.AgentChatEvent
	ch  chan protocol.AgentChatEvent
}

func newCCRecorder() *ccRecorder { return &ccRecorder{ch: make(chan protocol.AgentChatEvent, 1024)} }

func (r *ccRecorder) emit(ev protocol.AgentChatEvent) {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
	r.ch <- ev
}

func (r *ccRecorder) wait(t *testing.T, pred func(protocol.AgentChatEvent) bool) (protocol.AgentChatEvent, []protocol.AgentChatEvent) {
	t.Helper()
	var seen []protocol.AgentChatEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-r.ch:
			seen = append(seen, ev)
			if pred(ev) {
				return ev, seen
			}
		case <-deadline:
			t.Fatalf("timeout; saw %s", liveSummary(seen))
		}
	}
}

func isKind(k string) func(protocol.AgentChatEvent) bool {
	return func(ev protocol.AgentChatEvent) bool { return ev.Kind == k }
}

func readFixtureLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// fakeRunner answers one-shot commands by their joined args.
func fakeRunner(answers map[string]struct {
	out  string
	code int
}) ccRunner {
	return func(_ context.Context, spec ccSpec) ([]byte, int, error) {
		key := strings.Join(spec.Args, " ")
		for k, a := range answers {
			if strings.HasSuffix(key, k) {
				return []byte(a.out), a.code, nil
			}
		}
		return nil, 127, nil
	}
}
