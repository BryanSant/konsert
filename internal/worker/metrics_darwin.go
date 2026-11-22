//go:build darwin

package worker

import (
	"bufio"
	"bytes"
	"context"
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

	// Load average -> approximate CPU percent relative to cores
	if out, err := exec.CommandContext(ctx, "sysctl", "-n", "vm.loadavg").Output(); err == nil {
		fields := strings.Fields(strings.Trim(string(out), "{}\n\r "))
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

	// Total memory
	if out, err := exec.CommandContext(ctx, "sysctl", "-n", "hw.memsize").Output(); err == nil {
		if totalBytes, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); err == nil {
			m.MemTotalMB = totalBytes / (1024 * 1024)
		}
	}

	// Used memory via vm_stat
	if out, err := exec.CommandContext(ctx, "vm_stat").Output(); err == nil {
		var pageSize uint64 = 16384
		var active, wired, compressed uint64
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, "page size of") {
				for _, f := range strings.Fields(line) {
					if v, err := strconv.ParseUint(f, 10, 64); err == nil && v >= 4096 {
						pageSize = v
						break
					}
				}
				continue
			}
			val := parseVMStatVal(line)
			switch {
			case strings.HasPrefix(line, "Pages active:"):
				active = val
			case strings.HasPrefix(line, "Pages wired down:"):
				wired = val
			case strings.HasPrefix(line, "Pages occupied by compressor:"):
				compressed = val
			}
		}
		usedBytes := (active + wired + compressed) * pageSize
		m.MemUsedMB = usedBytes / (1024 * 1024)
	}

	// Listening TCP ports via lsof (non-privileged user processes)
	if out, err := exec.CommandContext(ctx, "lsof", "-iTCP", "-sTCP:LISTEN", "-nP", "-F", "cn").Output(); err == nil {
		var curProc string
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() {
			line := sc.Text()
			if len(line) < 2 {
				continue
			}
			switch line[0] {
			case 'c':
				curProc = line[1:]
			case 'n':
				addr := line[1:]
				if idx := strings.LastIndex(addr, ":"); idx >= 0 {
					if port, err := strconv.Atoi(addr[idx+1:]); err == nil && port >= 1024 && port <= 65535 {
						m.ListeningPorts = append(m.ListeningPorts, protocol.ListeningPort{
							Port:    port,
							Process: curProc,
							Address: addr[:idx],
						})
					}
				}
			}
		}
	}

	return m
}

func parseVMStatVal(line string) uint64 {
	_, rhs, ok := strings.Cut(line, ":")
	if !ok {
		return 0
	}
	rhs = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rhs), "."))
	v, _ := strconv.ParseUint(rhs, 10, 64)
	return v
}
