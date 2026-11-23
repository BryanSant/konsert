//go:build !windows

package gitutil

import "syscall"

// oNoFollow makes open(2) fail on a symlink in the final path component.
const oNoFollow = syscall.O_NOFOLLOW
