//go:build linux

package guard

import (
	"os"
	"path/filepath"
)

// DetectDesktop reports a graphical session on this host: an X11 socket or a Wayland
// socket in any user's runtime dir. A headless server gets the smaller reserve cap.
func DetectDesktop() bool {
	if entries, err := os.ReadDir("/tmp/.X11-unix"); err == nil && len(entries) > 0 {
		return true
	}
	if m, _ := filepath.Glob("/run/user/*/wayland-*"); len(m) > 0 {
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}
