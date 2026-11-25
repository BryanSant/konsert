package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/ids"
	"github.com/google/uuid"
)

// identityRecord is persisted at <stateDir>/worker-id-<name>-<isolation>.
type identityRecord struct {
	// ID is this daemon's UUIDv7: the hub's workers.id. The hub mints the short m- sid.
	ID string `json:"id"`
	// OSMachineHash fingerprints the machine the ID was generated on (/etc/machine-id on
	// Linux, IOPlatformUUID on macOS, MachineGuid on Windows). If it changes, the state dir was copied to another machine (a cloned
	// VM image), so a fresh ID is generated instead of impersonating the original.
	OSMachineHash string `json:"os_machine_hash,omitempty"`
}

// machineSource reads the platform's machine identity; tests replace it.
var machineSource = osMachineSource

// osMachineHash fingerprints the machine (see osMachineSource), or "" if it is unknown.
func osMachineHash() string {
	v := strings.TrimSpace(machineSource())
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("konsert:" + strings.ToLower(v)))
	return hex.EncodeToString(sum[:8])
}

// ResolveStableWorkerID returns this daemon's persistent identity: a UUIDv7 that is the
// hub's workers.id, so rerunning konsertd never registers a second machine row.
//
// Resolution order:
//  1. Read <baseDir>/worker-id-<name>-<isolation> (name defaults to the hostname). It is
//     reused while it holds a UUIDv7 generated on this OS installation.
//  2. Otherwise (absent, unreadable, or the OS machine hash changed because the state dir
//     was cloned) generate a new UUIDv7 and persist it atomically with 0600 permissions.
//  3. If it cannot be persisted (read-only filesystem), derive a deterministic UUID from
//     host facts so the identity is still stable across restarts.
func ResolveStableWorkerID(baseDir, name, hostGroup, isolation string) string {
	dir := strings.TrimSpace(baseDir)
	if dir == "" {
		dir = defaultStateDir()
	}
	if dir == "" {
		return deterministicWorkerID(name, hostGroup, isolation)
	}
	path := filepath.Join(dir, workerIDFilename(name, isolation))
	osHash := osMachineHash()

	if raw, err := os.ReadFile(path); err == nil {
		var rec identityRecord
		// The hash must match exactly: a record from another machine, or one written before
		// this platform had a fingerprint, gets a fresh identity.
		if json.Unmarshal(raw, &rec) == nil && ids.IsUUIDv7(rec.ID) && rec.OSMachineHash == osHash {
			return rec.ID
		}
	}

	rec := identityRecord{ID: ids.UUID(), OSMachineHash: osHash}
	raw, err := json.Marshal(rec)
	if err == nil && os.MkdirAll(dir, 0o700) == nil {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err == nil {
			if err := os.Rename(tmp, path); err == nil {
				return rec.ID
			}
			_ = os.Remove(tmp)
		}
	}
	return deterministicWorkerID(name, hostGroup, isolation)
}

// WorkerIDFor is the identity a daemon running with cfg uses: its explicit worker_id, else
// the persisted identity for its name and isolation (created on first use). `konsertd id`
// prints it, so the hub learns a host's workers.id before that daemon ever connects.
func WorkerIDFor(cfg config.WorkerConfig) string {
	if id := strings.TrimSpace(cfg.WorkerID); id != "" {
		return id
	}
	name := cfg.Name
	if name == "" {
		name, _ = os.Hostname()
		if name == "" {
			name = "worker"
		}
	}
	hostGroup := cfg.HostGroup
	if hostGroup == "" {
		hostGroup = name
	}
	return ResolveStableWorkerID(cfg.StateDir, name, hostGroup, cfg.Isolation)
}

// osHostname is this machine's hostname ("" if unknown).
func osHostname() string {
	h, _ := os.Hostname()
	return h
}

func workerIDFilename(name, isolation string) string {
	// Always keyed by name (default: the hostname) and isolation, never "the default
	// daemon": hosts sharing a home directory (a container, its siblings and its host)
	// each get their own file, and every host computes the same file for a given name,
	// so `konsertd uninstall` on one can tell the others' identities apart.
	cleanName := strings.TrimSpace(name)
	if cleanName == "" {
		cleanName, _ = os.Hostname()
	}
	cleanIso := strings.TrimSpace(isolation)
	if cleanIso == "" {
		cleanIso = "host"
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
		slug = "worker"
	}
	return "worker-id-" + slug
}

// deterministicWorkerID derives a stable RFC 9562 version-8 (custom) UUID from host facts,
// for a daemon that cannot persist its identity.
func deterministicWorkerID(name, hostGroup, isolation string) string {
	host, _ := os.Hostname()
	key := strings.Join([]string{
		strings.ToLower(strings.TrimSpace(host)),
		strings.ToLower(strings.TrimSpace(name)),
		strings.ToLower(strings.TrimSpace(hostGroup)),
		runtime.GOOS,
		runtime.GOARCH,
		strings.ToLower(strings.TrimSpace(isolation)),
		osMachineHash(),
	}, "|")
	sum := sha256.Sum256([]byte("konsert-worker:" + key))
	var u uuid.UUID
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x80 // version 8
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 9562 variant
	return u.String()
}

// LookupWorkerID is WorkerIDFor without creating anything: the explicit worker_id, else
// the persisted identity for cfg's name and isolation if there is one.
func LookupWorkerID(cfg config.WorkerConfig) (string, bool) {
	return lookupWorkerIDIn(defaultStateDir(), cfg)
}

// lookupWorkerIDIn is LookupWorkerID with the state directory given.
func lookupWorkerIDIn(dir string, cfg config.WorkerConfig) (string, bool) {
	if id := strings.TrimSpace(cfg.WorkerID); id != "" {
		return id, true
	}
	if dir == "" {
		return "", false
	}
	name := cfg.Name
	if name == "" {
		name, _ = os.Hostname()
	}
	raw, err := os.ReadFile(filepath.Join(dir, workerIDFilename(name, cfg.Isolation)))
	if err != nil {
		return "", false
	}
	var rec identityRecord
	if json.Unmarshal(raw, &rec) != nil || !ids.IsUUIDv7(rec.ID) {
		return "", false
	}
	return rec.ID, true
}
