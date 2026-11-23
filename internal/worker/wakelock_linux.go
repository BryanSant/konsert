//go:build linux

package worker

import (
	"os"
	"strconv"
	"strings"
)

// linuxIdleOnlyMethod is reported when only the idle inhibitor could be taken.
const linuxIdleOnlyMethod = "systemd-inhibit (idle only; blocking sleep needs a polkit rule, see docs/deploy.md)"

// defaultWakeBackend on Linux holds a logind idle+sleep inhibitor with systemd-inhibit,
// falling back to idle-only when polkit refuses the sleep inhibitor (non-console
// sessions). Instead of `sleep infinity`, the inhibited child is a small shell loop that
// exits once konsertd is gone, so a crashed konsertd never leaves the host pinned awake.
func defaultWakeBackend() wakeBackend {
	return &fallbackWakeBackend{
		primary:        systemdInhibitBackend("idle:sleep"),
		fallback:       systemdInhibitBackend("idle"),
		useFallback:    isPolkitDenied,
		fallbackMethod: linuxIdleOnlyMethod,
	}
}

func systemdInhibitBackend(what string) *commandWakeBackend {
	pid := strconv.Itoa(os.Getpid())
	return &commandWakeBackend{
		method: "systemd-inhibit",
		argv: func(reason string) []string {
			return []string{
				"systemd-inhibit",
				"--what=" + what,
				"--who=konsert",
				"--why=" + reason,
				"--mode=block",
				"/bin/sh", "-c", `while kill -0 "$0" 2>/dev/null; do sleep 15; done`, pid,
			}
		},
	}
}

// isPolkitDenied reports whether logind refused the inhibitor for lack of authorization.
func isPolkitDenied(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "access denied") || strings.Contains(msg, "interactive authentication")
}
