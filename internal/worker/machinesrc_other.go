//go:build !darwin && !windows

package worker

import (
	"os"
	"strings"
)

// osMachineIDPaths are read, first match wins, to fingerprint the OS installation.
var osMachineIDPaths = []string{"/etc/machine-id", "/var/lib/dbus/machine-id"}

// osMachineSource returns the systemd/dbus machine ID.
func osMachineSource() string {
	for _, p := range osMachineIDPaths {
		if raw, err := os.ReadFile(p); err == nil {
			if v := strings.TrimSpace(string(raw)); v != "" {
				return v
			}
		}
	}
	return ""
}
