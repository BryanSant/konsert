//go:build windows

package guard

import "os"

// rusagePeak: Windows peaks come from the Job Object instead.
func rusagePeak(*os.ProcessState) uint64 { return 0 }
