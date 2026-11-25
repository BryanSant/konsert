package controller

import (
	"context"
	"crypto/rand"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/secrets"
	"github.com/BryanSant/syre/internal/store"
)

// SecretKeyFileName is the default master-key file, created next to the database.
const SecretKeyFileName = "syre-secret.key"

// secretKeyFile resolves where the master key lives: cfg.SecretKeyFile, else next to
// the database. It returns "" when there is no on-disk database (tests, in-memory
// stores), in which case an ephemeral key is used.
func secretKeyFile(cfg config.ControllerConfig) string {
	if cfg.SecretKeyFile != "" {
		return cfg.SecretKeyFile
	}
	db := strings.TrimSpace(cfg.DBPath)
	if db == "" || db == ":memory:" || strings.HasPrefix(db, "file::memory:") {
		return ""
	}
	return filepath.Join(filepath.Dir(db), SecretKeyFileName)
}

// loadSecretBox loads (or creates) the master key. Without an on-disk database it uses
// a random in-memory key, so nothing is written to the working directory; secrets saved
// by such a controller only live as long as its store does anyway.
func loadSecretBox(cfg config.ControllerConfig, logger *slog.Logger) *secrets.Box {
	path := secretKeyFile(cfg)
	if path != "" {
		box, err := secrets.Load(path)
		if err == nil {
			return box
		}
		logger.Error("cannot load the secret key; secrets saved this run will not survive a restart", "path", path, "err", err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, _ := secrets.New(key)
	return box
}

// migratePlaintextSecrets encrypts an LLM API key saved in plaintext by an older build.
func migratePlaintextSecrets(st store.Store, box *secrets.Box, saved map[string]string, logger *slog.Logger) {
	v := strings.TrimSpace(saved["llm_api_key"])
	if v == "" || secrets.IsSealed(v) {
		return
	}
	sealed, err := box.Seal(v)
	if err != nil {
		logger.Error("cannot encrypt the saved LLM API key", "err", err)
		return
	}
	if err := st.SetSettings(context.Background(), map[string]string{"llm_api_key": sealed}); err != nil {
		logger.Error("cannot store the encrypted LLM API key", "err", err)
		return
	}
	saved["llm_api_key"] = sealed
	logger.Info("encrypted the saved LLM API key at rest")
}
