//go:build windows

package worker

import "os"

// %TEMP% is per-user on Windows, so the plain name is already private.
func scratchDirName() string { return "syre-attachments" }

func ownedByCurrentUser(os.FileInfo) bool { return true }
