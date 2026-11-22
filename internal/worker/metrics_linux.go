//go:build linux

package worker

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

func collectPlatformMetrics(parentCtx context.Context) *protocol.WorkerMetrics {
	ctx, cancel := context.WithTimeout(parentCtx, 2*time.Second)
	defer cancel()

	m := &protocol.WorkerMetrics{}

	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) >= 1 {
			if la, err := strconv.ParseFloat(fields[0], 64); err == nil {
				m.LoadAvg = la
				cpus := float64(runtime.NumCPU())
				if cpus > 0 {
					pct := (la / cpus) * 100
					if pct > 100 {
						pct = 100
					}
					m.CPUPercent = pct
				}
			}
		}
	}

	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		var totalKB, availKB uint64
		sc := bufio.NewScanner(bytes.NewReader(raw))
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 {
				continue
			}
			v, _ := strconv.ParseUint(fields[1], 10, 64)
			switch fields[0] {
			case "MemTotal:":
				totalKB = v
			case "MemAvailable:":
				availKB = v
			}
		}
		m.MemTotalMB = totalKB / 1024
		if totalKB >= availKB {
			m.MemUsedMB = (totalKB - availKB) / 1024
		}
	}

	// Best-effort listening TCP ports via ss
	if out, err := exec.CommandContext(ctx, "ss", "-tlnpH").Output(); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 4 {
				continue
			}
			local := fields[3]
			idx := strings.LastIndex(local, ":")
			if idx < 0 {
				continue
			}
			port, err := strconv.Atoi(local[idx+1:])
			if err != nil || port < 1024 || port > 65535 {
				continue
			}
			proc := ""
			if len(fields) >= 6 {
				if i := strings.Index(fields[5], "\""); i >= 0 {
					rest := fields[5][i+1:]
					if j := strings.Index(rest, "\""); j >= 0 {
						proc = rest[:j]
					}
				}
			}
			m.ListeningPorts = append(m.ListeningPorts, protocol.ListeningPort{
				Port:    port,
				Process: proc,
				Address: local[:idx],
			})
		}
	}

	return m
}
