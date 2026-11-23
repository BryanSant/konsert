package worker

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BryanSant/konsert/internal/ids"
)

// ResolveStableWorkerID returns a persistent, stable worker ID for this machine so rerunning
// konsertd never registers duplicate redundant machine rows on the controller.
//
// Resolution order:
//  1. Read existing persisted ID from <baseDir>/worker-id (or worker-id-<slug> if custom name/isolation).
//  2. If absent (or not an m- ID), generate a new m- ID and persist it atomically with 0600 permissions.
//  3. If the filesystem is read-only, fall back to a deterministic machine fingerprint ID.
func ResolveStableWorkerID(baseDir, name, hostGroup, isolation string) string {
	dir := strings.TrimSpace(baseDir)
	if dir == "" {
		dir = defaultStateDir()
	}

	filename := workerIDFilename(name, isolation)
	if dir != "" {
		idPath := filepath.Join(dir, filename)
		if raw, err := os.ReadFile(idPath); err == nil {
			// IDs from older builds (UUIDs) are replaced: pre-1.0 there is no compatibility.
			if existing := strings.TrimSpace(string(raw)); ids.Valid(ids.Machine, existing) {
				return existing
			}
		}
		newID, _ := ids.New(ids.Machine, nil) // the hub re-mints it on a clash
		if err := os.MkdirAll(dir, 0o700); err == nil {
			if err := os.WriteFile(idPath, []byte(newID+"\n"), 0o600); err == nil {
				return newID
			}
		}
	}

	return deterministicMachineID(name, hostGroup, isolation)
}

func workerIDFilename(name, isolation string) string {
	host, _ := os.Hostname()
	cleanName := strings.TrimSpace(name)
	cleanIso := strings.TrimSpace(isolation)
	if cleanIso == "" {
		cleanIso = "host"
	}
	// Default host daemon uses ~/.konsert/worker-id; custom named/container daemons get distinct persisted files.
	if (cleanName == "" || strings.EqualFold(cleanName, host)) && cleanIso == "host" {
		return "worker-id"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(cleanName + "-" + cleanIso) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	slug := strings.Trim(b.String(), "-_")
	if slug == "" {
		return "worker-id"
	}
	return "worker-id-" + slug
}

func deterministicMachineID(name, hostGroup, isolation string) string {
	host, _ := os.Hostname()
	key := strings.Join([]string{
		strings.ToLower(strings.TrimSpace(host)),
		strings.ToLower(strings.TrimSpace(name)),
		strings.ToLower(strings.TrimSpace(hostGroup)),
		runtime.GOOS,
		runtime.GOARCH,
		strings.ToLower(strings.TrimSpace(isolation)),
	}, "|")
	return ids.Hash(ids.Machine, key)
}
