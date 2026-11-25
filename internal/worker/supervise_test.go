package worker

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/drivers"
)

// lingeringPty models a PTY whose main process has exited while something (a background
// child on Unix, or ConPTY before ClosePseudoConsole) keeps Read blocked until Close.
type lingeringPty struct {
	once   sync.Once
	closed chan struct{}
	exit   int
}

func newLingeringPty(exit int) *lingeringPty {
	return &lingeringPty{closed: make(chan struct{}), exit: exit}
}

func (p *lingeringPty) Read([]byte) (int, error) {
	<-p.closed
	return 0, io.EOF
}
func (p *lingeringPty) Write(b []byte) (int, error) { return len(b), nil }
func (p *lingeringPty) Resize(uint16, uint16) error { return nil }
func (p *lingeringPty) Interrupt() error            { return nil }
func (p *lingeringPty) Terminate() error            { return p.Close() }
func (p *lingeringPty) Wait() (int, error)          { return p.exit, nil }
func (p *lingeringPty) Pid() int                    { return 0 }
func (p *lingeringPty) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

// Platform-independent check of the supervisor: completion follows Wait(), and the PTY is
// closed after a short drain so a reader that never sees EOF cannot hang the task.
func TestSuperviseCompletesWhenReadNeverEOFs(t *testing.T) {
	rec := newStatusRecorder()
	r := NewRunner(drivers.DefaultRegistry, false, nil, RunnerCallbacks{OnTaskStatus: rec.record}, nil)
	drv, _ := drivers.DefaultRegistry.Get("mock")
	ctx, cancel := context.WithCancel(context.Background())
	pty := newLingeringPty(0)
	rt := &runningTask{
		id:           "t-0wha9",
		effectiveDir: t.TempDir(),
		pty:          pty,
		ring:         NewRingBuffer(1024),
		cancelFunc:   cancel,
		status:       protocol.TaskStatusRunning,
	}
	r.mu.Lock()
	r.tasks[rt.id] = rt
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		r.superviseTask(ctx, rt, drv)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("superviseTask hung waiting for PTY EOF")
	}
	select {
	case <-pty.closed:
	default:
		t.Fatal("expected the PTY to be closed after the drain grace period")
	}
	var final *protocol.TaskStatusPayload
	for _, p := range rec.snapshot() {
		if protocol.IsTerminalTaskStatus(p.Status) {
			p := p
			final = &p
		}
	}
	if final == nil || final.Status != protocol.TaskStatusCompleted {
		t.Fatalf("expected completed status, got %+v", rec.snapshot())
	}
}
