package main

import (
	"fmt"
	"log/slog"
	"net"

	"github.com/BryanSant/syre/internal/config"
)

// prepareServeConfig finalizes the controller config before anything listens:
// it fails closed on missing tokens (generating and persisting them silently unless --no-auth),
// applies the auth-dependent default bind address, and warns about risky combinations.
func prepareServeConfig(cfg *config.ControllerConfig, logger *slog.Logger) error {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = config.DefaultListenAddr(cfg.NoAuth)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid controller config: %w", err)
	}
	if err := ensureAuthTokens(cfg); err != nil {
		return err
	}
	loopback := isLoopbackListenAddr(cfg.ListenAddr)
	if cfg.NoAuth && !loopback {
		logger.Warn("--no-auth on " + cfg.ListenAddr + ": anyone who can reach it controls your machines")
	}
	if cfg.TrustTailscaleHeaders && !loopback {
		logger.Warn("--trust-tailscale-headers only trusts loopback peers; listen on 127.0.0.1 behind Tailscale Serve")
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
