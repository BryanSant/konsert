package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/store"
)

// DeviceStore is the slice of the store the Manager needs for device credentials.
type DeviceStore interface {
	GetDeviceBySecretHash(ctx context.Context, hash string) (store.Device, error)
	TouchDevice(ctx context.Context, id string, at, expiresAt time.Time, ip, userAgent string) error
}

// DeviceSecretPrefix starts every device credential, so one is recognizable in a
// config file or a secret scanner.
const DeviceSecretPrefix = "kdv_"

const (
	// cacheTTL bounds how long a looked-up device is trusted without re-reading the
	// store. Revocation and rename invalidate the entry at once (Invalidate).
	cacheTTL = 30 * time.Second
	// touchEvery limits last-used/expiry writes to one per device per interval.
	touchEvery = 5 * time.Minute
	// cacheMax is the entry count above which stale entries are swept.
	cacheMax = 1024
)

type cachedDevice struct {
	dev     store.Device
	fetched time.Time
}

// NewDeviceSecret returns a new device credential and its hash (the only part stored).
func NewDeviceSecret() (secret, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	secret = DeviceSecretPrefix + base64.RawURLEncoding.EncodeToString(b)
	return secret, HashSecret(secret), nil
}

// HashSecret returns the hex SHA-256 of a device credential (or a poll secret).
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Invalidate drops any cached lookups of a device (after revoke or rename).
func (m *Manager) Invalidate(deviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, c := range m.cache {
		if c.dev.ID == deviceID {
			delete(m.cache, h)
		}
	}
}

// Expired reports whether a device is past its sliding expiry at now.
func (m *Manager) Expired(d store.Device, now time.Time) bool {
	return now.After(d.LastUsedAt.Add(m.IdleExpiry()))
}

// ExpiresAt is when an unused device expires under the current idle setting.
func (m *Manager) ExpiresAt(d store.Device) time.Time {
	return d.LastUsedAt.Add(m.IdleExpiry()).UTC()
}

func (m *Manager) lookupDevice(ctx context.Context, hash string, now time.Time) (store.Device, error) {
	m.mu.Lock()
	c, ok := m.cache[hash]
	m.mu.Unlock()
	if ok && now.Sub(c.fetched) < cacheTTL {
		return c.dev, nil
	}
	d, err := m.devices.GetDeviceBySecretHash(ctx, hash)
	if err != nil {
		return d, err
	}
	m.mu.Lock()
	if len(m.cache) >= cacheMax {
		for h, e := range m.cache {
			if now.Sub(e.fetched) >= cacheTTL {
				delete(m.cache, h)
			}
		}
	}
	m.cache[hash] = cachedDevice{dev: d, fetched: now}
	m.mu.Unlock()
	return d, nil
}

// authenticateDevice verifies a device credential and slides its expiry.
func (m *Manager) authenticateDevice(r *http.Request, secret string) (Identity, bool) {
	if m.devices == nil || !strings.HasPrefix(secret, DeviceSecretPrefix) || len(secret) > 128 {
		return Identity{}, false
	}
	now := m.Now()
	hash := HashSecret(secret)
	d, err := m.lookupDevice(r.Context(), hash, now)
	if err != nil || d.RevokedAt != nil || m.Expired(d, now) {
		return Identity{}, false
	}
	if now.Sub(d.LastUsedAt) >= touchEvery {
		ip, ua := ClientIP(r), UserAgentSummary(r.UserAgent())
		exp := now.Add(m.IdleExpiry())
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		err := m.devices.TouchDevice(ctx, d.ID, now, exp, ip, ua)
		cancel()
		if err == nil {
			d.LastUsedAt, d.ExpiresAt, d.LastIP, d.LastUserAgent = now, exp, ip, ua
			m.mu.Lock()
			if c, ok := m.cache[hash]; ok && c.dev.ID == d.ID {
				c.dev = d
				m.cache[hash] = c
			}
			m.mu.Unlock()
		}
	}
	return Identity{User: DeviceUser(d), Method: MethodDevice, Device: &d}, true
}

// DeviceUser is how a device appears as the user in the audit log.
func DeviceUser(d store.Device) string {
	return d.Name + " (" + d.SID + ")"
}

// ClientIP returns the direct peer IP of the request (no proxy headers are trusted).
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- Pairing codes ---

// codeAlphabet is upper-case Crockford base32 (no I, L, O, U).
const codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// PairingCodeTTL is how long a pairing or login code stays valid.
const PairingCodeTTL = 10 * time.Minute

// NewPairingCode returns a random XXXX-XXXX code (40 bits) and its hash.
func NewPairingCode() (code, hash string, err error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	for i, c := range b {
		b[i] = codeAlphabet[int(c)%32] // 256 is a multiple of 32: no modulo bias
	}
	canon := string(b)
	return canon[:4] + "-" + canon[4:], HashPairingCode(canon), nil
}

// ErrBadCode is returned for input that cannot be a pairing code.
var ErrBadCode = errors.New("not a pairing code")

// NormalizePairingCode turns user input ("7kq2 m9tx", "7KQ2-M9TX") into the canonical
// 8-character code, applying Crockford's substitutions (I, L → 1; O → 0).
func NormalizePairingCode(s string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r == '-' || r == ' ':
			continue
		case r == 'I' || r == 'L':
			r = '1'
		case r == 'O':
			r = '0'
		}
		if r > 127 || !strings.ContainsRune(codeAlphabet, r) {
			return "", ErrBadCode
		}
		b.WriteRune(r)
		if b.Len() > 8 {
			return "", ErrBadCode
		}
	}
	if b.Len() != 8 {
		return "", ErrBadCode
	}
	return b.String(), nil
}

// HashPairingCode returns the stored hash of a canonical code.
func HashPairingCode(canonical string) string {
	sum := sha256.Sum256([]byte("konsert-pairing-code:" + canonical))
	return hex.EncodeToString(sum[:])
}

// --- User agents ---

// UserAgentSummary reduces a User-Agent header to "Browser on OS" (or a short tool
// name such as "konsert/1.2"), so the hub never stores the raw header.
func UserAgentSummary(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return ""
	}
	if !strings.Contains(ua, "Mozilla/") {
		// Non-browser clients ("konsert-cli/dev", "Go-http-client/1.1", "curl/8.4").
		name, _, _ := strings.Cut(ua, " ")
		if len(name) > 40 {
			name = name[:40]
		}
		return name
	}
	browser := "Browser"
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "OPR/"):
		browser = "Opera"
	case strings.Contains(ua, "Firefox/") || strings.Contains(ua, "FxiOS/"):
		browser = "Firefox"
	case strings.Contains(ua, "Chrome/") || strings.Contains(ua, "CriOS/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	}
	os := ""
	switch {
	case strings.Contains(ua, "iPhone"):
		os = "iPhone"
	case strings.Contains(ua, "iPad"):
		os = "iPad"
	case strings.Contains(ua, "Android"):
		os = "Android"
	case strings.Contains(ua, "Mac OS X") || strings.Contains(ua, "Macintosh"):
		os = "macOS"
	case strings.Contains(ua, "Windows"):
		os = "Windows"
	case strings.Contains(ua, "CrOS"):
		os = "ChromeOS"
	case strings.Contains(ua, "Linux"):
		os = "Linux"
	}
	if os == "" {
		return browser
	}
	return browser + " on " + os
}

// DeviceKindForUA guesses browser vs phone from a User-Agent header.
func DeviceKindForUA(ua string) string {
	if strings.Contains(ua, "iPhone") || strings.Contains(ua, "Mobile") || strings.Contains(ua, "Android") {
		return store.DeviceKindPhone
	}
	return store.DeviceKindBrowser
}
