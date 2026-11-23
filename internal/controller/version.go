package controller

import "strings"

// NormalizeVersion returns v without surrounding space or a leading "v", or ""
// when the version is unknown (empty, "dev" or "unknown", as in local builds
// that were not stamped via ldflags).
func NormalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "", "dev", "unknown", "(devel)":
		return ""
	}
	return strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
}

// VersionMismatch reports whether a worker's konsertd version differs from the
// controller's. Unknown versions on either side never count as a mismatch.
func VersionMismatch(controllerVersion, workerVersion string) bool {
	c, w := NormalizeVersion(controllerVersion), NormalizeVersion(workerVersion)
	return c != "" && w != "" && c != w
}
