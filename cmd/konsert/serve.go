package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/controller"
	"github.com/BryanSant/konsert/internal/store/sqlite"
)

// shutdownTimeout bounds how long shutdown waits for in-flight work to drain.
const shutdownTimeout = 10 * time.Second

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
