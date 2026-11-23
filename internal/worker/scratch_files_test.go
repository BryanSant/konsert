package worker

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestResolveScratchDirByOS(t *testing.T) {
	if got := ResolveScratchDir("darwin"); !strings.HasPrefix(got, "/private/var/") {
		t.Fatalf("expected macOS /private/var/ scratch path, got %q", got)
	}
	if got := ResolveScratchDir("linux"); !strings.HasPrefix(got, "/var/tmp/") {
		t.Fatalf("expected Linux /var/tmp/ scratch path, got %q", got)
	}
	if got := ResolveScratchDir("windows"); !strings.HasSuffix(got, "konsert-attachments") {
		t.Fatalf("expected Windows temp scratch path, got %q", got)
	}
}

func TestSanitizeAttachmentFilename(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":    "passwd",
		"C:\\temp\\notes.txt": "notes.txt",
		"my file (1).png":     "my_file__1_.png",
		"":                    "attachment.bin",
		"...":                 "attachment.bin",
	}
	for in, want := range cases {
		if got := SanitizeAttachmentFilename(in); got != want {
			t.Fatalf("SanitizeAttachmentFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSaveAndPruneScratchAttachment(t *testing.T) {
	dir := t.TempDir()

	// Seed an expired file
	stale := filepath.Join(dir, "old-file.txt")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(stale, oldTime, oldTime)

	savedPath, err := SaveScratchAttachment(dir, "sess-abc", "../screenshot.png", []byte("png-bytes"))
	if err != nil {
		t.Fatalf("SaveScratchAttachment failed: %v", err)
	}
	if !strings.HasSuffix(savedPath, "screenshot.png") {
		t.Fatalf("unexpected saved path: %q", savedPath)
	}
	info, err := os.Stat(savedPath)
	if err != nil {
		t.Fatalf("stat saved attachment failed: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("expected restrictive 0600 permissions, got %o", info.Mode().Perm())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("expected stale attachment to be pruned, stat err=%v", err)
	}
}
