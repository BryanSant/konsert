//go:build !darwin && !linux

package worker

import (
	"context"
	"os/exec"
	"runtime"
	"strings"

	"github.com/BryanSant/syre/internal/protocol"
)

func collectPlatformFacts(ctx context.Context, f *protocol.MachineFacts) {
	f.OSName = runtime.GOOS
	if runtime.GOOS == "windows" {
		f.OSName = "Windows"
		if out, err := exec.CommandContext(ctx, "cmd", "/C", "ver").Output(); err == nil {
			f.OSVersion = strings.TrimSpace(string(out))
		}
	}
}
