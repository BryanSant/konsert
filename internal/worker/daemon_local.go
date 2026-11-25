package worker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/linkwatch"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/coder/websocket"
)

// runLocal serves the hub on the local socket (no controller URL configured): the hub
// attaches, directly or through `syred attach` over SSH, and gets exactly the
// session an outbound connection would. Sessions belong to the daemon, so a dropped
// link or a restarted hub only ends the connection; the next attach sends worker.hello
// with the live tasks and the hub resyncs. A newer attach replaces an older one (a
// half-open SSH link the hub already gave up on). Removal by the hub (close 4001)
// uninstalls this identity (service, that hub's authorized_keys grant, hooks if it was
// the account's last syred) and stops the daemon.
func (d *Daemon) runLocal(ctx context.Context) error {
	path := d.SocketPath
	if path == "" {
		path = LocalSocketPath(d.stateDir(), d.cfg.WorkerID)
	}
	ln, err := listenLocal(path)
	if err != nil {
		d.shutdown()
		return err
	}
	srvCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	removed := make(chan string, 1)
	var removedOnce sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+protocol.AttachPath, func(w http.ResponseWriter, r *http.Request) {
		lv := linkwatch.New(d.linkConfig(), nil)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OnPingReceived: func(context.Context, []byte) bool { lv.Received(); return true },
			OnPongReceived: func(context.Context, []byte) { lv.Received() },
		})
		if err != nil {
			d.logger.Warn("attach: websocket upgrade failed", "err", err)
			return
		}
		hubID := r.Header.Get(protocol.HubIDHeader)
		peer := linkPeer{transport: attachTransport(r.Header.Get(protocol.TransportHeader)), addr: "local socket"}
		if d.serveAttached(ctx, srvCtx, conn, peer, lv) {
			removedOnce.Do(func() { removed <- hubID })
		}
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return srvCtx },
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	d.logger.Info("listening on " + path)
	d.link.waiting("")

	var runErr error
	removedBy, wasRemoved := "", false
	select {
	case <-ctx.Done():
		runErr = ctx.Err()
	case removedBy = <-removed:
		wasRemoved = true
		d.logger.Info("removed from the hub; uninstalling and stopping")
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}
	cancel()
	_ = srv.Close()
	d.shutdown()
	if wasRemoved {
		d.removedByHub(ctx, removedBy)
	}
	return runErr
}

// serveAttached serves one attached connection, replacing the current one. It reports
// whether the hub removed this machine.
func (d *Daemon) serveAttached(daemonCtx, srvCtx context.Context, conn *websocket.Conn, peer linkPeer, lv *linkwatch.Liveness) bool {
	connCtx, cancel := context.WithCancel(srvCtx)
	done := make(chan struct{})
	d.attachMu.Lock()
	prevCancel, prevDone := d.attachCancel, d.attachDone
	d.attachCancel, d.attachDone = cancel, done
	d.attachMu.Unlock()
	if prevCancel != nil {
		// The old connection's reads end at once; its close handshake is bounded. Wait
		// for it, so two connections never share the daemon's send queues.
		prevCancel()
		<-prevDone
	}
	defer func() {
		cancel()
		d.attachMu.Lock()
		if d.attachDone == done {
			d.attachCancel, d.attachDone = nil, nil
		}
		d.attachMu.Unlock()
		close(done)
	}()

	err := d.serveConn(daemonCtx, connCtx, conn, peer, lv)
	if websocket.CloseStatus(err) == websocket.StatusCode(protocol.WSCloseWorkerRemoved) {
		return true
	}
	if daemonCtx.Err() == nil {
		// Keep a host wake lock through short outages; drop it if the hub stays away.
		d.wake.Disconnected(d.WakeLockGrace)
		msg := "hub detached; waiting for it to attach again"
		if errors.Is(err, errLinkDead) {
			msg = "hub link dead (" + err.Error() + "); waiting for it to attach again"
		}
		d.logger.Info(msg)
		d.logger.Debug("attach ended", "err", err)
		// A newer attach may already have replaced this one.
		d.attachMu.Lock()
		current := d.attachDone == done
		d.attachMu.Unlock()
		if current {
			errText := ""
			if err != nil {
				errText = err.Error()
			}
			d.link.waiting(errText)
		}
	}
	return false
}

// attachTransport names the transport an attaching hub reported (ssh or local).
func attachTransport(h string) string {
	switch h {
	case "ssh", "local":
		return h
	}
	return "local socket"
}
