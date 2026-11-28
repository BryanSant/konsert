//go:build !linux && !darwin && !windows

package worker

import (
	"context"
	"time"
)

func platformUserIdle(context.Context) (time.Duration, string, bool) { return 0, "", false }
