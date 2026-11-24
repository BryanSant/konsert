package controller

import (
	"context"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/coder/websocket"
)

// TestHubDetectsSilentWorker: a daemon that stops answering (it neither sends nor reads,
// so pings go unanswered) is dropped after LinkDead and marked offline at once.
func TestHubDetectsSilentWorker(t *testing.T) {
	th := newTestHub(t)
	th.h.LinkQuiet = 50 * time.Millisecond
	th.h.LinkDead = 300 * time.Millisecond
	th.h.StaleWorkerTimeout = time.Hour
	const id = "silent-worker"
	th.dialWorkerOpts(t, testWorkerInfo(id, "silent"), nil, true)
	waitFor(t, 5*time.Second, "worker to register", func() bool { return th.h.IsWorkerConnected(id) })
	start := time.Now()
	waitFor(t, 5*time.Second, "worker marked offline", func() bool {
		w, err := th.st.GetWorker(context.Background(), id)
		return err == nil && w.Status == "offline"
	})
	if got := time.Since(start); got < 250*time.Millisecond {
		t.Fatalf("offline after %v, before the link could be dead", got)
	}
	if th.h.IsWorkerConnected(id) {
		t.Fatal("dead link still registered")
	}
}

// TestHubKeepsQuietButLiveWorker: a daemon that sends nothing but answers pings stays
// connected and online.
func TestHubKeepsQuietButLiveWorker(t *testing.T) {
	th := newTestHub(t)
	th.h.LinkQuiet = 50 * time.Millisecond
	th.h.LinkDead = 300 * time.Millisecond
	const id = "quiet-worker"
	th.connectWorker(t, testWorkerInfo(id, "quiet"), nil, id)
	time.Sleep(1500 * time.Millisecond) // 5x LinkDead
	if !th.h.IsWorkerConnected(id) {
		t.Fatal("a live link was dropped")
	}
	if w, err := th.st.GetWorker(context.Background(), id); err != nil || w.Status != "online" {
		t.Fatalf("status %q, err %v", w.Status, err)
	}
}

// TestHubWorkerPortsOnDemand: the hub asks the daemon for its ports and caches the
// answer; with the daemon gone it serves the cached list.
func TestHubWorkerPortsOnDemand(t *testing.T) {
	th := newTestHub(t)
	const id = "ports-worker"
	fw := th.connectWorker(t, testWorkerInfo(id, "ports"), nil, id)
	type result struct {
		p     protocol.WorkerPortsPayload
		fresh bool
		err   error
	}
	got := make(chan result, 1)
	go func() {
		p, fresh, err := th.h.WorkerPorts(context.Background(), id)
		got <- result{p, fresh, err}
	}()
	req := fw.expect(protocol.MsgWorkerPortsRequest, 5*time.Second)
	raw, err := protocol.NewEnvelope(protocol.MsgWorkerPorts, req.ID, protocol.WorkerPortsPayload{
		Ports: []protocol.ListeningPort{{Port: 5173, Process: "node"}}, CollectedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fw.conn.Write(context.Background(), websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	r := <-got
	if r.err != nil || !r.fresh || len(r.p.Ports) != 1 || r.p.Ports[0].Port != 5173 {
		t.Fatalf("ports %+v fresh %v err %v", r.p, r.fresh, r.err)
	}
	// An unsolicited update replaces the cache; with the daemon gone, the cache answers.
	fw.send(protocol.MsgWorkerPorts, protocol.WorkerPortsPayload{Ports: []protocol.ListeningPort{{Port: 8080}}})
	waitFor(t, 3*time.Second, "cached ports", func() bool {
		p, ok := th.h.ports.get(id)
		return ok && len(p.Ports) == 1 && p.Ports[0].Port == 8080
	})
	fw.close()
	waitFor(t, 3*time.Second, "worker to disconnect", func() bool { return !th.h.IsWorkerConnected(id) })
	p, fresh, err := th.h.WorkerPorts(context.Background(), id)
	if err != nil || fresh || len(p.Ports) != 1 || p.Ports[0].Port != 8080 {
		t.Fatalf("cached ports %+v fresh %v err %v", p, fresh, err)
	}
}
