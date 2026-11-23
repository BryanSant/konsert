package main

import (
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/BryanSant/konsert/internal/config"
)

// prepareServeConfig finalizes the controller config before anything listens:
// it fails closed on missing tokens (generating and persisting them unless --no-auth),
// applies the auth-dependent default bind address, and warns about risky combinations.
func prepareServeConfig(cfg *config.ControllerConfig, out io.Writer, logger *slog.Logger) error {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = config.DefaultListenAddr(cfg.NoAuth)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid controller config: %w", err)
	}
	if err := ensureAuthTokens(cfg, out); err != nil {
		return err
	}
	loopback := isLoopbackListenAddr(cfg.ListenAddr)
	if cfg.NoAuth && !loopback {
		logger.Warn("!!! AUTHENTICATION IS DISABLED (--no-auth) ON A NON-LOOPBACK ADDRESS !!! "+
			"Anyone who can reach this port can run arbitrary commands on every connected worker. "+
			"Bind to 127.0.0.1 or drop --no-auth.",
			"addr", cfg.ListenAddr)
	}
	if cfg.TrustTailscaleHeaders && !loopback {
		logger.Warn("--trust-tailscale-headers is set but the listener is not loopback-only; "+
			"Tailscale-User-Login is honored only from loopback peers (Tailscale Serve), "+
			"so bind to 127.0.0.1 and let Tailscale Serve front it",
			"addr", cfg.ListenAddr)
	}
	return nil
}

// isLoopbackListenAddr reports whether addr binds only to a loopback interface.
// An empty host (":8420") means all interfaces.
func isLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
