package worker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	MaxAttachmentBytes = 12 * 1024 * 1024 // 12 MiB raw file limit
	AttachmentTTL      = 24 * time.Hour
)

// ResolveScratchDir returns the OS-appropriate scratch directory for chat file attachments:
//   - macOS:   /private/var/tmp/konsert-attachments (falls back to os.TempDir())
//   - Linux:   /var/tmp/konsert-attachments (falls back to os.TempDir())
//   - Windows: %TEMP%\konsert-attachments
func ResolveScratchDir(goos string) string {
	if goos == "" {
		goos = runtime.GOOS
	}
	switch goos {
	case "darwin":
		return "/private/var/tmp/konsert-attachments"
	case "linux":
		return "/var/tmp/konsert-attachments"
	case "windows":
		return filepath.Join(os.TempDir(), "konsert-attachments")
	default:
		return filepath.Join(os.TempDir(), "konsert-attachments")
	}
}

// SanitizeAttachmentFilename strips path components and unsafe characters while preserving the base name and extension.
func SanitizeAttachmentFilename(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.ReplaceAll(raw, "\\", "/")
	base := filepath.Base(raw)
	if base == "" || base == "." || base == "/" || base == ".." {
		return "attachment.bin"
	}
	var b strings.Builder
	for _, r := range base {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	clean := strings.Trim(b.String(), "._")
	if clean == "" {
		return "attachment.bin"
	}
	if len(clean) > 120 {
		ext := filepath.Ext(clean)
		stem := strings.TrimSuffix(clean, ext)
		if len(ext) > 16 {
			ext = ext[:16]
		}
		if len(stem) > 100 {
			stem = stem[:100]
		}
		clean = stem + ext
	}
	return clean
}

// SaveScratchAttachment writes data into an OS-specific scratch directory with 0700 dir / 0600 file permissions
// and performs best-effort pruning of stale files older than AttachmentTTL.
func SaveScratchAttachment(baseDir, sessionID, filename string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("attachment is empty")
	}
	if len(data) > MaxAttachmentBytes {
		return "", fmt.Errorf("attachment exceeds maximum size of %d bytes", MaxAttachmentBytes)
	}

	dir := strings.TrimSpace(baseDir)
	if dir == "" {
		dir = ResolveScratchDir(runtime.GOOS)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// Fallback to os.TempDir() if /private/var/tmp or /var/tmp is read-only (e.g. restricted container).
		fallback := filepath.Join(os.TempDir(), "konsert-attachments")
		if fErr := os.MkdirAll(fallback, 0o700); fErr != nil {
			return "", fmt.Errorf("create scratch dir: %w", err)
		}
		dir = fallback
	}

	PruneScratchAttachments(dir, AttachmentTTL)

	safeName := SanitizeAttachmentFilename(filename)
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	prefix := fmt.Sprintf("%d-%s", time.Now().UTC().Unix(), hex.EncodeToString(rnd[:]))
	if sid := SanitizeAttachmentFilename(sessionID); sid != "" && sid != "attachment.bin" {
		if len(sid) > 12 {
			sid = sid[:12]
		}
		prefix = sid + "-" + prefix
	}

	destPath := filepath.Join(dir, prefix+"-"+safeName)
	if err := os.WriteFile(destPath, data, 0o600); err != nil {
		return "", fmt.Errorf("write scratch attachment: %w", err)
	}
	absPath, err := filepath.Abs(destPath)
	if err == nil {
		return absPath, nil
	}
	return destPath, nil
}

// PruneScratchAttachments removes attachment files older than maxAge inside baseDir.
func PruneScratchAttachments(baseDir string, maxAge time.Duration) {
	if maxAge <= 0 {
		maxAge = AttachmentTTL
	}
	dir := strings.TrimSpace(baseDir)
	if dir == "" {
		dir = ResolveScratchDir(runtime.GOOS)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}
