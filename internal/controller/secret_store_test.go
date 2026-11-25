package controller

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/secrets"
	"github.com/BryanSant/syre/internal/store/sqlite"
)

func secretTestServer(t *testing.T, dbPath string) (*Server, func()) {
	t.Helper()
	st, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	cfg.DBPath = dbPath
	return NewServer(cfg, st, nil, "test"), func() { _ = st.Close() }
}

func TestLLMAPIKeyEncryptedAtRest(t *testing.T) {
	t.Setenv(secrets.KeyEnv, "") // use the key file next to the database
	dbPath := filepath.Join(t.TempDir(), "syre.db")
	ctx := context.Background()

	srv, closeStore := secretTestServer(t, dbPath)
	key := "sk-ant-very-secret"
	if err := srv.applySettings(ctx, SettingsUpdate{LLMProvider: ptr("anthropic"), LLMAPIKey: &key}, true); err != nil {
		t.Fatal(err)
	}
	saved, _ := srv.store.GetAllSettings(ctx)
	if raw := saved["llm_api_key"]; !secrets.IsSealed(raw) || strings.Contains(raw, key) {
		t.Fatalf("API key stored in the clear: %q", raw)
	}
	keyFile := filepath.Join(filepath.Dir(dbPath), SecretKeyFileName)
	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("secret key file not created next to the database: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("secret key file mode = %v", info.Mode().Perm())
	}
	closeStore()

	// A restarted controller decrypts the key with the same key file.
	srv2, closeStore2 := secretTestServer(t, dbPath)
	defer closeStore2()
	srv2.cfgMu.RLock()
	got := srv2.cfg.Commander.APIKey
	srv2.cfgMu.RUnlock()
	if got != key {
		t.Fatalf("restarted controller API key = %q, want the saved key", got)
	}
}

func TestPlaintextAPIKeyMigratedOnStartup(t *testing.T) {
	t.Setenv(secrets.KeyEnv, "")
	dbPath := filepath.Join(t.TempDir(), "syre.db")
	ctx := context.Background()
	st, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSettings(ctx, map[string]string{"llm_provider": "anthropic", "llm_api_key": "sk-legacy-plain"}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	srv, closeStore := secretTestServer(t, dbPath)
	defer closeStore()
	saved, _ := srv.store.GetAllSettings(ctx)
	if !secrets.IsSealed(saved["llm_api_key"]) {
		t.Fatalf("legacy plaintext key not encrypted on startup: %q", saved["llm_api_key"])
	}
	srv.cfgMu.RLock()
	defer srv.cfgMu.RUnlock()
	if srv.cfg.Commander.APIKey != "sk-legacy-plain" {
		t.Fatalf("migrated key not usable: %q", srv.cfg.Commander.APIKey)
	}
}

func ptr(s string) *string { return &s }
