//go:build !windows

package worker

import "github.com/BryanSant/syre/internal/protocol"

// collectStartMode is Windows-only (MachineFacts.StartMode).
func collectStartMode(*protocol.MachineFacts) {}
