//go:build darwin

package guard

import "os"

func selfFootprint() (uint64, bool) { return physFootprint(os.Getpid()) }
