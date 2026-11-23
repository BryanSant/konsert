// Package secrets encrypts small secrets (such as the orchestrator LLM API key) before
// they are written to the database, so a copied or backed-up konsert.db does not leak
// them.
//
// Values are sealed with AES-256-GCM under a 32-byte master key and stored as
// "enc:v1:<base64(nonce || ciphertext)>". The master key comes from $KONSERT_SECRET_KEY
// (base64) when set, otherwise from a key file (default konsert-secret.key next to the
// database) that is created with mode 0600 on first use.
//
// This protects the database at rest. It does not protect against an attacker who can
// read the controller's files, since the key file lives beside the database unless the
// key is supplied through the environment.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeyEnv supplies the master key directly (base64 of 32 bytes), instead of a key file.
const KeyEnv = "KONSERT_SECRET_KEY"

const prefix = "enc:v1:"

// Box seals and opens secrets with a master key.
type Box struct {
	aead cipher.AEAD
}

// New returns a Box for a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secret key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Load returns a Box using $KONSERT_SECRET_KEY if set, otherwise the key in keyFile,
// creating that file (0600, parent directories 0700) with a fresh random key if it does
// not exist yet.
func Load(keyFile string) (*Box, error) {
	if v := strings.TrimSpace(os.Getenv(KeyEnv)); v != "" {
		key, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("%s: not valid base64: %w", KeyEnv, err)
		}
		return New(key)
	}
	if keyFile == "" {
		return nil, errors.New("no secret key file configured")
	}
	if raw, err := os.ReadFile(keyFile); err == nil {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, fmt.Errorf("secret key file %s: not valid base64: %w", keyFile, err)
		}
		return New(key)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read secret key file: %w", err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		return nil, fmt.Errorf("create secret key directory: %w", err)
	}
	// O_EXCL: if another process created the file meanwhile, use theirs.
	f, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return Load(keyFile)
	}
	if err != nil {
		return nil, fmt.Errorf("create secret key file: %w", err)
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n")
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(keyFile)
		return nil, fmt.Errorf("write secret key file: %w", errors.Join(werr, cerr))
	}
	return New(key)
}

// Seal encrypts plaintext. The empty string stays empty (meaning "not set").
func (b *Box) Seal(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(plaintext), []byte(prefix))
	return prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a value produced by Seal. A value without the "enc:v1:" prefix is a
// legacy plaintext value and is returned unchanged (see IsSealed to migrate it).
func (b *Box) Open(stored string) (string, error) {
	if !IsSealed(stored) {
		return stored, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, prefix))
	if err != nil {
		return "", fmt.Errorf("decode sealed secret: %w", err)
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("sealed secret is truncated")
	}
	pt, err := b.aead.Open(nil, raw[:ns], raw[ns:], []byte(prefix))
	if err != nil {
		return "", errors.New("cannot decrypt secret: wrong or rotated secret key")
	}
	return string(pt), nil
}

// IsSealed reports whether a stored value was produced by Seal.
func IsSealed(stored string) bool { return strings.HasPrefix(stored, prefix) }
