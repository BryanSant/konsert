package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BryanSant/konsert/internal/auth"
	"github.com/BryanSant/konsert/internal/config"
	"gopkg.in/yaml.v3"
)

// tokensFileName is the default name of the hub's token file, stored next to the
// SQLite database.
const tokensFileName = "konsert-tokens.yaml"

// persistedTokens is the tokens file: the hidden recovery key (never printed).
// Machines have their own credentials (konsertd join), so there is no fleet token.
type persistedTokens struct {
	RecoveryKey string `yaml:"recovery_key,omitempty"`
}

// resolveTokensFile returns the path used to persist the hub's tokens.
func resolveTokensFile(cfg config.ControllerConfig) string {
	if cfg.TokensFile != "" {
		return cfg.TokensFile
	}
	return filepath.Join(filepath.Dir(cfg.DBPath), tokensFileName)
}

// readTokensFile reads the tokens file; a missing file is empty, not an error.
// A file readable by others is tightened to 0600.
func readTokensFile(path string) (persistedTokens, error) {
	var saved persistedTokens
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(raw, &saved); err != nil {
			return saved, fmt.Errorf("parse tokens file %s: %w", path, err)
		}
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(path, 0o600); err != nil {
				return saved, fmt.Errorf("chmod tokens file %s: %w", path, err)
			}
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return saved, fmt.Errorf("read tokens file %s: %w", path, err)
	}
	return saved, nil
}

// ensureAuthTokens makes the controller fail closed. Unless --no-auth is set, a
// missing recovery key is loaded from the tokens file or, failing that, generated and persisted there with mode 0600. Nothing is printed: people sign
// in by pairing (the startup sign-in link), and the recovery key stays in the file
// for emergencies.
func ensureAuthTokens(cfg *config.ControllerConfig) error {
	if cfg.NoAuth {
		return nil
	}
	if cfg.RecoveryKey != "" {
		return nil
	}
	path := resolveTokensFile(*cfg)
	saved, err := readTokensFile(path)
	if err != nil {
		return err
	}
	generated := false
	fill := func(dst *string, persisted *string, name string) error {
		if *dst != "" {
			return nil
		}
		if *persisted == "" {
			tok, err := auth.GenerateToken(32)
			if err != nil {
				return fmt.Errorf("generate %s: %w", name, err)
			}
			*persisted = tok
			generated = true
		}
		*dst = *persisted
		return nil
	}
	if cfg.RecoveryKey == "" {
		cfg.RecoveryKeyInTokensFile = true
	}
	if err := fill(&cfg.RecoveryKey, &saved.RecoveryKey, "recovery key"); err != nil {
		return err
	}
	if !generated {
		return nil
	}
	return writeTokensFile(path, saved)
}

// rotateRecoveryKeyInFile replaces the recovery key in the tokens file, keeping the
// rest of it.
func rotateRecoveryKeyInFile(path, newKey string) error {
	saved, err := readTokensFile(path)
	if err != nil {
		return err
	}
	saved.RecoveryKey = newKey
	return writeTokensFile(path, saved)
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
