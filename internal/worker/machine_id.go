package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"

	"github.com/BryanSant/konsert/internal/ids"
)

// machineIDRecord is persisted at <stateDir>/machine-id[-<slug>].
type machineIDRecord struct {
	MachineID string `json:"machine_id"`
	// OSMachineHash fingerprints the OS machine identity (/etc/machine-id on Linux) the ID
	// was generated on. If it changes, the state dir was copied to another machine (a cloned
	// VM image), so a fresh ID is generated instead of impersonating the original.
	OSMachineHash string `json:"os_machine_hash,omitempty"`
}

// osMachineIDPaths are read, first match wins, to fingerprint the OS installation.
var osMachineIDPaths = []string{"/etc/machine-id", "/var/lib/dbus/machine-id"}

func osMachineHash() string {
	for _, p := range osMachineIDPaths {
		if raw, err := os.ReadFile(p); err == nil {
			if v := strings.TrimSpace(string(raw)); v != "" {
				sum := sha256.Sum256([]byte("konsert:" + v))
				return hex.EncodeToString(sum[:8])
			}
		}
	}
	return ""
}

// ResolveMachineID returns this konsertd installation's stable machine UUID, sent to the hub
// as WorkerInfo.MachineID so two hosts with the same hostname are never conflated. It is
// generated once and persisted (0600) in baseDir (default ~/.konsert), one file per daemon
// identity (name/isolation, like the worker-id file). It returns "" if it cannot be
// persisted, so the hub falls back to its legacy matching rather than seeing a new
// identity on every start.
func ResolveMachineID(baseDir, name, isolation string) string {
	dir := strings.TrimSpace(baseDir)
	if dir == "" {
		dir = defaultStateDir()
	}
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, strings.Replace(workerIDFilename(name, isolation), "worker-id", "machine-id", 1))
	osHash := osMachineHash()

	if raw, err := os.ReadFile(path); err == nil {
		var rec machineIDRecord
		if json.Unmarshal(raw, &rec) == nil {
			if ids.IsUUIDv7(rec.MachineID) && (rec.OSMachineHash == "" || osHash == "" || rec.OSMachineHash == osHash) {
				return rec.MachineID
			}
		}
	}

	rec := machineIDRecord{MachineID: ids.UUID(), OSMachineHash: osHash}
	raw, err := json.Marshal(rec)
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return ""
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return ""
	}
	return rec.MachineID
}
