//go:build darwin

package guard

// DetectDesktop: every Mac runs WindowServer.
func DetectDesktop() bool { return true }
