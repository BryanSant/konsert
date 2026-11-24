//go:build darwin

package worker

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"

	"github.com/BryanSant/konsert/internal/protocol"
)

// collectPlatformPorts lists listening TCP ports with lsof (non-privileged user
// processes). It forks, so it runs only on demand and every few minutes, never per
// heartbeat.
func collectPlatformPorts(ctx context.Context) []protocol.ListeningPort {
	out, err := exec.CommandContext(ctx, "lsof", "-iTCP", "-sTCP:LISTEN", "-nP", "-F", "cn").Output()
	if err != nil {
		return nil
	}
	return parseLsofPorts(out)
}

func parseLsofPorts(out []byte) []protocol.ListeningPort {
	var ports []protocol.ListeningPort
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
					ports = append(ports, protocol.ListeningPort{Port: port, Process: curProc, Address: addr[:idx]})
				}
			}
		}
	}
	return ports
}
