package worker

import (
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GPU utilization for machine heat, only where it is cheap to read without privileges:
// macOS IOAccelerator statistics (ioreg, ~15 ms) and nvidia-smi on Linux. Elsewhere (and
// when the tool is missing) it stays unknown.

var (
	nvidiaSMIOnce sync.Once
	nvidiaSMIPath string
)

var ioregUtilRe = regexp.MustCompile(`"Device Utilization %"\s*=\s*(\d+)`)

// collectGPUPercent returns the busiest GPU's utilization (0-100), or nil when unknown.
func collectGPUPercent(parent context.Context) *float64 {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.CommandContext(ctx, "ioreg", "-r", "-d", "1", "-w", "0", "-c", "IOAccelerator").Output()
		if err != nil {
			return nil
		}
		return parseIORegGPU(string(out))
	case "linux":
		nvidiaSMIOnce.Do(func() { nvidiaSMIPath, _ = exec.LookPath("nvidia-smi") })
		if nvidiaSMIPath == "" {
			return nil
		}
		out, err := exec.CommandContext(ctx, nvidiaSMIPath, "--query-gpu=utilization.gpu", "--format=csv,noheader,nounits").Output()
		if err != nil {
			return nil
		}
		return parseNvidiaSMIGPU(string(out))
	}
	return nil
}

// parseIORegGPU takes the highest "Device Utilization %" of the IOAccelerator entries.
func parseIORegGPU(out string) *float64 {
	var best *float64
	for _, m := range ioregUtilRe.FindAllStringSubmatch(out, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && v >= 0 && v <= 100 {
			if best == nil || v > *best {
				best = &v
			}
		}
	}
	return best
}

// parseNvidiaSMIGPU takes the highest utilization line of nvidia-smi's CSV output.
func parseNvidiaSMIGPU(out string) *float64 {
	var best *float64
	for _, line := range strings.Split(out, "\n") {
		if v, err := strconv.ParseFloat(strings.TrimSpace(line), 64); err == nil && v >= 0 && v <= 100 {
			if best == nil || v > *best {
				best = &v
			}
		}
	}
	return best
}
