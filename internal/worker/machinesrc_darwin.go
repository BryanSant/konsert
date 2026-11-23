package worker

import (
	"context"
	"os/exec"
	"regexp"
	"time"
)

var ioPlatformUUIDRe = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([0-9A-Fa-f-]{36})"`)

// osMachineSource returns the Mac's hardware UUID (IOPlatformUUID), which survives OS
// reinstalls but differs on every machine and VM, so a cloned state dir is detected.
func osMachineSource() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return ""
	}
	return parseIOPlatformUUID(string(out))
}

func parseIOPlatformUUID(ioregOutput string) string {
	if m := ioPlatformUUIDRe.FindStringSubmatch(ioregOutput); m != nil {
		return m[1]
	}
	return ""
}
