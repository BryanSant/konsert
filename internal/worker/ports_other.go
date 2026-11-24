//go:build !darwin && !linux

package worker

import (
	"context"

	"github.com/BryanSant/konsert/internal/protocol"
)

// collectPlatformPorts is not implemented here.
func collectPlatformPorts(context.Context) []protocol.ListeningPort { return nil }
