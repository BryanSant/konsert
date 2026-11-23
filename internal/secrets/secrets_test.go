package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	t.Setenv(KeyEnv, "")
	b, err := Load(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := b.Seal("sk-ant-secret")
	if err != nil {
		t.Fatal(err)
	}
	if !IsSealed(sealed) || strings.Contains(sealed, "sk-ant-secret") {
		t.Fatalf("value not sealed: %q", sealed)
	}
	again, _ := b.Seal("sk-ant-secret")
	if again == sealed {
		t.Fatal("sealing must use a fresh nonce each time")
	}
	got, err := b.Open(sealed)
	if err != nil || got != "sk-ant-secret" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if s, _ := b.Seal(""); s != "" {
		t.Fatalf("empty value must stay empty, got %q", s)
	}
}

func TestLegacyPlaintextPassesThrough(t *testing.T) {
	t.Setenv(KeyEnv, "")
	b, _ := Load(filepath.Join(t.TempDir(), "k"))
	if got, err := b.Open("sk-plain"); err != nil || got != "sk-plain" {
		t.Fatalf("Open(plain) = %q, %v", got, err)
	}
}

func TestKeyFilePersistsAndIsPrivate(t *testing.T) {
	t.Setenv(KeyEnv, "")
	path := filepath.Join(t.TempDir(), "sub", "konsert-secret.key")
	b1, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := b1.Seal("v")
	b2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b2.Open(sealed); err != nil || got != "v" {
		t.Fatalf("reloaded key cannot open: %q, %v", got, err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode = %v, want 0600", info.Mode().Perm())
		}
	}
}

func TestWrongKeyFailsClearly(t *testing.T) {
	t.Setenv(KeyEnv, "")
	b1, _ := Load(filepath.Join(t.TempDir(), "a"))
	b2, _ := Load(filepath.Join(t.TempDir(), "b"))
	sealed, _ := b1.Seal("v")
	if _, err := b2.Open(sealed); err == nil || !strings.Contains(err.Error(), "wrong or rotated") {
		t.Fatalf("expected a clear wrong-key error, got %v", err)
	}
	if _, err := b1.Open(sealed[:len(sealed)-4] + "AAAA"); err == nil {
		t.Fatal("tampered ciphertext must not open")
	}
}

func TestKeyFromEnvironment(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	t.Setenv(KeyEnv, base64.StdEncoding.EncodeToString(key))
	path := filepath.Join(t.TempDir(), "unused.key")
	b, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("no key file should be created when the key comes from the environment")
	}
	sealed, _ := b.Seal("v")
	direct, _ := New(key)
	if got, err := direct.Open(sealed); err != nil || got != "v" {
		t.Fatalf("env key mismatch: %q, %v", got, err)
	}
	t.Setenv(KeyEnv, "not-base64!")
	if _, err := Load(path); err == nil {
		t.Fatal("invalid env key must error")
	}
}
