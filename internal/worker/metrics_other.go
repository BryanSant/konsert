//go:build !darwin && !linux

package worker

import (
	"context"

	"github.com/BryanSant/konsert/internal/protocol"
)

func collectPlatformMetrics(_ context.Context) *protocol.WorkerMetrics {
	return &protocol.WorkerMetrics{}
}
