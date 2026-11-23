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

// ResolveScratchDir returns the OS-appropriate, per-user scratch directory for chat file
// attachments (the shared /var/tmp would let another user pre-create the directory and have
// pruning delete their files, or read ours):
//   - macOS:   /private/var/tmp/konsert-attachments-<uid> (falls back to os.TempDir())
//   - Linux:   /var/tmp/konsert-attachments-<uid> (falls back to os.TempDir())
//   - Windows: %TEMP%\konsert-attachments (%TEMP% is already per-user)
func ResolveScratchDir(goos string) string {
	if goos == "" {
		goos = runtime.GOOS
	}
	switch goos {
	case "darwin":
		return "/private/var/tmp/" + scratchDirName()
	case "linux":
		return "/var/tmp/" + scratchDirName()
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
		dir = attachmentsDir()
	}
	if err := ensurePrivateDir(dir); err != nil {
		// Fallback to os.TempDir() if /private/var/tmp or /var/tmp is read-only (e.g. restricted
		// container) or the directory there is not ours.
		fallback := filepath.Join(os.TempDir(), scratchDirName())
		if fErr := ensurePrivateDir(fallback); fErr != nil {
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
		dir = attachmentsDir()
	}
	if err := checkPrivateDir(dir); err != nil {
		return // never prune a directory we do not own
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
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

// ensurePrivateDir creates dir (0700) if needed and verifies it is safe to use.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return checkPrivateDir(dir)
}

// checkPrivateDir verifies dir is a real directory (not a symlink) owned by the current user,
// tightening its permissions to 0700 if they are broader.
func checkPrivateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if !ownedByCurrentUser(info) {
		return fmt.Errorf("%s is not owned by the current user", dir)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("restrict %s: %w", dir, err)
		}
	}
	return nil
}

// AttachmentsDirEnv overrides the attachments directory (tests, side-by-side daemons).
const AttachmentsDirEnv = "KONSERT_ATTACHMENTS_DIR"

// attachmentsDir is $KONSERT_ATTACHMENTS_DIR, else the per-OS scratch directory.
func attachmentsDir() string {
	if v := strings.TrimSpace(os.Getenv(AttachmentsDirEnv)); v != "" {
		return v
	}
	return ResolveScratchDir(runtime.GOOS)
}
