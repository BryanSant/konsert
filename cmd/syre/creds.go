package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/controller"
	"gopkg.in/yaml.v3"
)

// How the syre CLI finds a hub and a credential without anyone copying a token:
//
//   - URL: --url, else $SYRE_URL, else the hub last signed in to with `syre login`,
//     else the hub running on this machine (recorded by `syre serve` in local-hub.yaml),
//     else http://127.0.0.1:8420.
//   - Credential: --token, else $SYRE_TOKEN, else the device credential `syre login`
//     saved for that URL (credentials.yaml), else, for a loopback URL only, the hub's
//     recovery key from its tokens file (readable only by the user running the hub).
//
// Both files live in the user config dir (os.UserConfigDir()/syre, or
// $SYRE_CONFIG_DIR) with mode 0600.

const (
	credentialsFileName = "credentials.yaml"
	localHubFileName    = "local-hub.yaml"
	defaultHubURL       = "http://127.0.0.1:8420"
)

// configDir is where the CLI keeps credentials.
func configDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("SYRE_CONFIG_DIR")); d != "" {
		return d, nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "syre"), nil
}

// savedCredential is one hub's device credential.
type savedCredential struct {
	Token     string    `yaml:"token"`
	DeviceSID string    `yaml:"device,omitempty"`
	Name      string    `yaml:"name,omitempty"`
	SavedAt   time.Time `yaml:"saved_at,omitempty"`
}

// credentialsFile maps hub URLs to device credentials.
type credentialsFile struct {
	// Current is the hub commands use when no --url or $SYRE_URL is given.
	Current string                     `yaml:"current,omitempty"`
	Hubs    map[string]savedCredential `yaml:"hubs,omitempty"`
}

// localHub is what `syre serve` records so the CLI on the same machine finds it.
type localHub struct {
	URL        string `yaml:"url"`
	TokensFile string `yaml:"tokens_file"`
}

// normalizeHubURL canonicalizes a hub URL for use as a key ("hub:8420" → "http://hub:8420").
func normalizeHubURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty hub URL")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("invalid hub URL %q", raw)
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.Path, "/"), nil
}

// isLoopbackURL reports whether a hub URL points at this machine.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func readYAMLFile(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(raw, v)
}

// writePrivateYAML writes v to path atomically with mode 0600 (dir 0700).
func writePrivateYAML(path string, v any) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func loadCredentials() (credentialsFile, string, error) {
	var c credentialsFile
	dir, err := configDir()
	if err != nil {
		return c, "", err
	}
	path := filepath.Join(dir, credentialsFileName)
	if err := readYAMLFile(path, &c); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return c, path, fmt.Errorf("read %s: %w", path, err)
	}
	if c.Hubs == nil {
		c.Hubs = map[string]savedCredential{}
	}
	return c, path, nil
}

func saveCredentials(c credentialsFile, path string) error {
	if err := writePrivateYAML(path, c); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	return nil
}

func loadLocalHub() localHub {
	var h localHub
	if dir, err := configDir(); err == nil {
		_ = readYAMLFile(filepath.Join(dir, localHubFileName), &h)
	}
	return h
}

// recordLocalHub notes where the hub on this machine listens and keeps its tokens.
func recordLocalHub(listenAddr, tokensFile string) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return err
	}
	host := "127.0.0.1"
	if h, _, _ := net.SplitHostPort(listenAddr); h != "" && h != "::" && h != "0.0.0.0" {
		if ip := net.ParseIP(h); ip == nil || !ip.IsUnspecified() {
			host = h
		}
	}
	if abs, err := filepath.Abs(tokensFile); err == nil {
		tokensFile = abs
	}
	return writePrivateYAML(filepath.Join(dir, localHubFileName), localHub{
		URL: "http://" + net.JoinHostPort(host, port), TokensFile: tokensFile,
	})
}

// localTokensFile finds the tokens file of the hub on this machine.
func localTokensFile(flag string) string {
	if flag != "" {
		return flag
	}
	if v := strings.TrimSpace(os.Getenv("SYRE_TOKENS_FILE")); v != "" {
		return v
	}
	if h := loadLocalHub(); h.TokensFile != "" {
		return h.TokensFile
	}
	db := "syre.db"
	if v := strings.TrimSpace(os.Getenv("SYRE_DB_PATH")); v != "" {
		db = v
	}
	return filepath.Join(filepath.Dir(db), tokensFileName)
}

// hubTarget is a resolved hub URL and credential.
type hubTarget struct {
	URL   string
	Token string
	// Source says where the credential came from ("flag", "env", "login",
	// "tokens file") for error hints; "" when there is none.
	Source string
}

// resolveHub applies the lookup order documented at the top of this file.
func resolveHub(urlFlag, tokenFlag, tokensFileFlag string) hubTarget {
	creds, _, _ := loadCredentials()
	var t hubTarget
	switch {
	case urlFlag != "":
		t.URL = urlFlag
	case os.Getenv("SYRE_URL") != "":
		t.URL = os.Getenv("SYRE_URL")
	case creds.Current != "":
		t.URL = creds.Current
	default:
		if h := loadLocalHub(); h.URL != "" {
			t.URL = h.URL
		} else {
			t.URL = defaultHubURL
		}
	}
	if n, err := normalizeHubURL(t.URL); err == nil {
		t.URL = n
	}
	switch {
	case tokenFlag != "":
		t.Token, t.Source = tokenFlag, "flag"
	case os.Getenv("SYRE_TOKEN") != "":
		t.Token, t.Source = os.Getenv("SYRE_TOKEN"), "env"
	case creds.Hubs[t.URL].Token != "":
		t.Token, t.Source = creds.Hubs[t.URL].Token, "login"
	case isLoopbackURL(t.URL):
		// Only ever sent to this machine: the recovery key never leaves it.
		if saved, err := readTokensFile(localTokensFile(tokensFileFlag)); err == nil && saved.RecoveryKey != "" {
			t.Token, t.Source = saved.RecoveryKey, "tokens file"
		}
	}
	return t
}

// newHubClient returns a REST client for a resolved hub.
func newHubClient(t hubTarget, source string) *controller.RemoteFleetClient {
	c := controller.NewRemoteFleetClient(t.URL, t.Token)
	c.Source = source
	c.UserAgent = "syre/" + version
	return c
}
