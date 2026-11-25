//go:build linux

package worker

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"

	"github.com/BryanSant/syre/internal/protocol"
)

// collectPlatformPorts lists listening TCP ports with ss (best effort). It forks, so it
// runs only on demand and every few minutes, never per heartbeat.
func collectPlatformPorts(ctx context.Context) []protocol.ListeningPort {
	out, err := exec.CommandContext(ctx, "ss", "-tlnpH").Output()
	if err != nil {
		return nil
	}
	return parseSSPorts(out)
}

func parseSSPorts(out []byte) []protocol.ListeningPort {
	var ports []protocol.ListeningPort
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
		ports = append(ports, protocol.ListeningPort{Port: port, Process: proc, Address: local[:idx]})
	}
	return ports
}
