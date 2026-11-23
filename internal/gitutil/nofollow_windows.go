//go:build windows

package gitutil

// oNoFollow is unavailable on Windows; the Lstat and SameFile checks still apply.
const oNoFollow = 0
