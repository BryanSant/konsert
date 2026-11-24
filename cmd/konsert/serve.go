package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/controller"
	"github.com/BryanSant/konsert/internal/store/sqlite"
)

// shutdownTimeout bounds how long shutdown waits for in-flight work to drain.
const shutdownTimeout = 10 * time.Second

// launchGrace is how long after startup the hub waits for configured daemons to
// reconnect before starting the ones that did not (a variable for tests).
var launchGrace = 15 * time.Second

// runServe runs the controller until ctx is cancelled (or the listener fails), then
// shuts down in order: cancel the server-lifetime context, close hub sockets, drain
// HTTP handlers (including hijacked WebSocket handlers), wait for background
// goroutines, and only then close the store. onListening, if non-nil, receives the
// bound address (used by tests listening on port 0).
func runServe(ctx context.Context, cfg config.ControllerConfig, logger *slog.Logger, onListening func(net.Addr)) (err error) {
	// Background goroutines get their own lifetime context so they are stopped
	// explicitly during shutdown, after request handlers have drained.
	lifeCtx, cancelLife := context.WithCancel(context.Background())
	defer cancelLife()

	if err := prepareDBFile(cfg.DBPath); err != nil {
		return err
	}
	st, err := sqlite.Open(lifeCtx, cfg.DBPath)
	if err != nil {
		return err
	}
	// Deferred first, so it runs last: after every handler and goroutine is done.
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close store: %w", cerr)
		}
	}()
	if err := secureDBFiles(cfg.DBPath); err != nil {
		logger.Warn("could not restrict database file permissions", "db", cfg.DBPath, "err", err)
	}

	srv := controller.NewServer(cfg, st, logger, version)
	var bg sync.WaitGroup
	bg.Go(func() { srv.Hub().RunReaper(lifeCtx) })

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		cancelLife()
		bg.Wait()
		return err
	}
	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return lifeCtx },
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()

	logger.Info("konsert controller listening",
		"addr", ln.Addr().String(),
		"db", cfg.DBPath,
		"mcp_http", cfg.EnableMCPHTTP,
		"auth_enabled", !cfg.NoAuth,
	)
	srv.SetListenAddr(ln.Addr().String())
	if cfg.RecoveryKeyInTokensFile {
		path := resolveTokensFile(cfg)
		srv.SetRecoveryKeyRotator(func(k string) error { return rotateRecoveryKeyInFile(path, k) })
	}
	if !cfg.NoAuth {
		announceSignIn(lifeCtx, srv, cfg, ln.Addr().String(), logger)
	}
	// Start configured daemons (local and SSH hosts) that do not reconnect on their own.
	srv.StartLaunchReconciler(launchGrace)
	if onListening != nil {
		onListening(ln.Addr())
	}

	var runErr error
	select {
	case <-ctx.Done():
	case e := <-serveErr:
		if e != nil && !errors.Is(e, http.ErrServerClosed) {
			runErr = e
		}
	}

	logger.Info("konsert controller shutting down")
	shCtx, shCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shCancel()
	// Cancel the server-lifetime context first: request contexts derive from it
	// (BaseContext), so long-polls and socket loops unwind, and the reaper stops.
	cancelLife()
	// Close worker/browser sockets so their (hijacked) handlers return.
	srv.Hub().Close()
	if err := httpSrv.Shutdown(shCtx); err != nil {
		logger.Warn("http shutdown did not complete", "err", err)
	}
	if err := srv.WaitIdle(shCtx); err != nil {
		logger.Warn("request handlers still running at shutdown deadline", "err", err)
	}
	bg.Wait()
	return runErr
}

// signInQROut receives the startup sign-in QR code when it is a terminal (a variable
// for tests).
var signInQROut io.Writer = os.Stderr

// announceSignIn mints a pairing code at startup and prints one short line with its
// link (plus a QR code in a terminal), so the first browser or phone signs in by
// opening the link: nobody copies a token.
func announceSignIn(ctx context.Context, srv *controller.Server, cfg config.ControllerConfig, listenAddr string, logger *slog.Logger) {
	base, loopbackOnly := controller.BestBaseURL(listenAddr, cfg.PublicURL)
	pc, err := srv.MintPairingCode(ctx, base, "hub startup", "")
	if err != nil {
		logger.Warn("could not create a sign-in link; run konsert pair", "err", err)
		return
	}
	note := "10 min"
	if loopbackOnly {
		note = "10 min, this machine only"
	}
	logger.Info(fmt.Sprintf("Sign in: %s (%s)", pc.URL, note))
	writeQR(signInQROut, pc.URL)
}
