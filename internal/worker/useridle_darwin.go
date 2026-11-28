package worker

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"time"
)

// platformUserIdle reads IOHIDSystem's HIDIdleTime (nanoseconds since the last input
// event, machine-wide) with ioreg: there is no cgo-free API for it.
func platformUserIdle(ctx context.Context) (time.Duration, string, bool) {
	out, err := exec.CommandContext(ctx, "ioreg", "-c", "IOHIDSystem", "-d", "4", "-k", "HIDIdleTime").Output()
	if err != nil {
		return 0, "", false
	}
	return parseHIDIdle(out)
}

func parseHIDIdle(out []byte) (time.Duration, string, bool) {
	i := bytes.Index(out, []byte(`"HIDIdleTime" = `))
	if i < 0 {
		return 0, "", false
	}
	rest := out[i+len(`"HIDIdleTime" = `):]
	end := bytes.IndexAny(rest, "\r\n ")
	if end < 0 {
		end = len(rest)
	}
	ns, err := strconv.ParseInt(string(rest[:end]), 10, 64)
	if err != nil || ns < 0 {
		return 0, "", false
	}
	return time.Duration(ns), "hid", true
}
