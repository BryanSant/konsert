package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/BryanSant/konsert/internal/auth"
	"github.com/BryanSant/konsert/internal/config"
	"gopkg.in/yaml.v3"
)

// tokensFileName is the default name of the auto-generated token file, stored next to
// the SQLite database.
const tokensFileName = "konsert-tokens.yaml"

type persistedTokens struct {
	AdminToken string `yaml:"admin_token,omitempty"`
	FleetToken string `yaml:"fleet_token,omitempty"`
}

// resolveTokensFile returns the path used to persist auto-generated tokens.
func resolveTokensFile(cfg config.ControllerConfig) string {
	if cfg.TokensFile != "" {
		return cfg.TokensFile
	}
	return filepath.Join(filepath.Dir(cfg.DBPath), tokensFileName)
}

// ensureAuthTokens makes the controller fail closed. Unless --no-auth is set, any
// missing admin or fleet token is loaded from the tokens file or, failing that,
// generated and persisted there with mode 0600. Newly generated tokens are printed
// once to out so the operator can log in and enroll workers.
func ensureAuthTokens(cfg *config.ControllerConfig, out io.Writer) error {
	if cfg.NoAuth {
		return nil
	}
	if cfg.AdminToken != "" && cfg.FleetToken != "" {
		return nil
	}
	path := resolveTokensFile(*cfg)

	var saved persistedTokens
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(raw, &saved); err != nil {
			return fmt.Errorf("parse tokens file %s: %w", path, err)
		}
		// Tighten permissions on a file that was copied or created loosely.
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(path, 0o600); err != nil {
				return fmt.Errorf("chmod tokens file %s: %w", path, err)
			}
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return fmt.Errorf("read tokens file %s: %w", path, err)
	}

	var generated []string
	fill := func(dst *string, persisted *string, name string) error {
		if *dst != "" {
			return nil
		}
		if *persisted != "" {
			*dst = *persisted
			return nil
		}
		tok, err := auth.GenerateToken(32)
		if err != nil {
			return fmt.Errorf("generate %s: %w", name, err)
		}
		*dst, *persisted = tok, tok
		generated = append(generated, name)
		return nil
	}
	if err := fill(&cfg.AdminToken, &saved.AdminToken, "admin token"); err != nil {
		return err
	}
	if err := fill(&cfg.FleetToken, &saved.FleetToken, "fleet token"); err != nil {
		return err
	}
	if len(generated) == 0 {
		return nil
	}

	if err := writeTokensFile(path, saved); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n=== konsert: no auth tokens were configured; generated new ones ===\n")
	fmt.Fprintf(out, "Saved to %s (mode 0600). They are shown only this once:\n", path)
	if slices.Contains(generated, "admin token") {
		fmt.Fprintf(out, "  admin token (PWA login, REST, MCP): %s\n", saved.AdminToken)
	}
	if slices.Contains(generated, "fleet token") {
		fmt.Fprintf(out, "  fleet token (konsertd --token):     %s\n", saved.FleetToken)
	}
	fmt.Fprintf(out, "Pass --admin-token/--fleet-token (or KONSERT_*_TOKEN) to choose your own, or --no-auth for isolated local development.\n\n")
	return nil
}

func writeTokensFile(path string, t persistedTokens) error {
	data, err := yaml.Marshal(t)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create tokens dir: %w", err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write tokens file: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod tokens file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("persist tokens file: %w", err)
	}
	return nil
}
