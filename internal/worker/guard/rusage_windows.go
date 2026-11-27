//go:build windows

package guard

import "os"

// rusagePeak: Windows peaks come from the Job Object instead.
func rusagePeak(*os.ProcessState) uint64 { return 0 }

// killedBySIGKILL: Windows has no OOM killer; allocations fail instead.
func killedBySIGKILL(*os.ProcessState) bool { return false }
