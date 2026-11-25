package worker

import (
	"context"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/drivers"
	"github.com/coder/websocket"
)

// syred reconnects after 1013 (try again later) and 1000 (superseded), and exits only on
// 4001 (machine removed by an operator).
func TestDaemonCloseCodePolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	hub := newFakeHub(t)
	d := NewDaemon(config.WorkerConfig{
		ControllerURL: hub.wsURL(), WorkerID: "close-test", Name: "close-test", Tags: map[string]string{},
	}, drivers.DefaultRegistry, discardLogger())
	d.HeartbeatInterval = time.Hour
	d.FactsInterval = time.Hour
	removals := make(chan string, 1)
	d.selfRemove = func(_ context.Context, hubID string) (bool, error) {
		removals <- hubID
		return false, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(ctx) }()

	for _, code := range []websocket.StatusCode{protocol.WSCloseTryAgainLater, websocket.StatusNormalClosure} {
		c := hub.accept(t)
		c.waitFor(t, 30*time.Second, "worker.hello", isType(protocol.MsgWorkerHello))
		_ = c.conn.Close(code, "test")
		select {
		case err := <-runErr:
			t.Fatalf("daemon exited after close code %d: %v", code, err)
		case <-time.After(100 * time.Millisecond):
		}
	}

	c := hub.accept(t)
	c.waitFor(t, 30*time.Second, "worker.hello", isType(protocol.MsgWorkerHello))
	_ = c.conn.Close(websocket.StatusCode(protocol.WSCloseWorkerRemoved), "removed")
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("expected clean exit on 4001, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not exit after 4001")
	}
	// An outbound (manual) machine uninstalls itself too; it has no hub key grant.
	select {
	case hubID := <-removals:
		if hubID != "" {
			t.Fatalf("outbound self-removal got hub id %q", hubID)
		}
	default:
		t.Fatal("4001 must make an outbound daemon uninstall itself")
	}
}
