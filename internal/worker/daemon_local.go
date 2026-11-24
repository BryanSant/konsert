package worker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/hubkey"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/coder/websocket"
)

// runLocal serves the hub on the local socket (no controller URL configured): the hub
// attaches, directly or through `konsertd attach` over SSH, and gets exactly the
// session an outbound connection would. Sessions belong to the daemon, so a dropped
// link or a restarted hub only ends the connection; the next attach sends worker.hello
// with the live tasks and the hub resyncs. A newer attach replaces an older one (a
// half-open SSH link the hub already gave up on). Removal by the hub (close 4001) drops
// that hub's authorized_keys grant for this identity and stops the daemon.
func (d *Daemon) runLocal(ctx context.Context) error {
	path := d.SocketPath
	if path == "" {
		path = LocalSocketPath("", d.cfg.WorkerID)
	}
	ln, err := listenLocal(path)
	if err != nil {
		d.shutdown()
		return err
	}
	srvCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	removed := make(chan struct{})
	var removedOnce sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+protocol.AttachPath, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			d.logger.Warn("attach: websocket upgrade failed", "err", err)
			return
		}
		hubID := r.Header.Get(protocol.HubIDHeader)
		if d.serveAttached(ctx, srvCtx, conn) {
			d.dropHubGrant(hubID)
			removedOnce.Do(func() { close(removed) })
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

	var runErr error
	select {
	case <-ctx.Done():
		runErr = ctx.Err()
	case <-removed:
		d.logger.Info("removed from the hub; stopping")
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}
	cancel()
	_ = srv.Close()
	d.shutdown()
	return runErr
}

// serveAttached serves one attached connection, replacing the current one. It reports
// whether the hub removed this machine.
func (d *Daemon) serveAttached(daemonCtx, srvCtx context.Context, conn *websocket.Conn) bool {
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

	err := d.serveConn(daemonCtx, connCtx, conn, "local socket")
	if websocket.CloseStatus(err) == websocket.StatusCode(protocol.WSCloseWorkerRemoved) {
		return true
	}
	if daemonCtx.Err() == nil {
		// Keep a host wake lock through short outages; drop it if the hub stays away.
		d.wake.Disconnected(d.WakeLockGrace)
		d.logger.Info("hub detached; waiting for it to attach again")
		d.logger.Debug("attach ended", "err", err)
	}
	return false
}

// dropHubGrant removes this identity from hubID's authorized_keys line, so the hub
// that removed the machine can no longer attach.
func (d *Daemon) dropHubGrant(hubID string) {
	if !hubkey.ValidHubID(hubID) || !hubkey.ValidWorkerID(d.cfg.WorkerID) {
		return
	}
	path, changed, err := hubkey.Uninstall(hubID, d.cfg.WorkerID)
	switch {
	case err != nil:
		d.logger.Warn("could not remove the hub key", "file", path, "err", err)
	case changed:
		d.logger.Info("removed the hub key from " + path)
	}
}
