package worker

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// platformUserIdle asks logind for this user's sessions on a seat (graphical or a
// local console). Their IdleHint is set by the desktop once its own idle timeout
// passes (or, on a text console, from the tty's last input), so it is coarse: a user
// active within the desktop's timeout reads as 0. Machines with no seat session
// (servers reached over SSH) report nothing.
func platformUserIdle(ctx context.Context) (time.Duration, string, bool) {
	out, err := exec.CommandContext(ctx, "loginctl", "list-sessions", "--no-legend").Output()
	if err != nil {
		return 0, "", false
	}
	uid := strconv.Itoa(os.Getuid())
	var best time.Duration = -1
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// SESSION UID USER SEAT ...: only sessions on a seat have a person at them.
		if len(f) < 4 || f[1] != uid || !strings.HasPrefix(f[3], "seat") {
			continue
		}
		props, err := exec.CommandContext(ctx, "loginctl", "show-session", f[0], "-p", "IdleHint", "-p", "IdleSinceHint", "-p", "Active").Output()
		if err != nil {
			continue
		}
		if idle, ok := parseLogindIdle(props, time.Now()); ok && (best < 0 || idle < best) {
			best = idle
		}
	}
	if best < 0 {
		return 0, "", false
	}
	return best, "logind", true
}

// parseLogindIdle reads IdleHint / IdleSinceHint (µs since the epoch) / Active.
func parseLogindIdle(props []byte, now time.Time) (time.Duration, bool) {
	kv := map[string]string{}
	for line := range strings.SplitSeq(string(props), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			kv[k] = v
		}
	}
	if kv["Active"] == "no" {
		return 0, false // a switched-away session: someone else may be at the seat
	}
	if kv["IdleHint"] != "yes" {
		return 0, true
	}
	us, err := strconv.ParseInt(kv["IdleSinceHint"], 10, 64)
	if err != nil || us <= 0 {
		return 0, true
	}
	return max(0, now.Sub(time.UnixMicro(us))), true
}
