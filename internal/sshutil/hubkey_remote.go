package sshutil

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RemoveHubKey removes workerID from hubID's authorized_keys line on target (the whole
// line once no machine is left), over the user's own SSH access: the hub key itself
// may only attach. The hub does this when it removes a machine it is not connected to
// (a connected konsertd drops the grant itself). A host without konsertd has no line.
func RemoveHubKey(parent context.Context, target, hubID, workerID string) error {
	t, err := ParseTarget(target)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	sess := &session{t: t}
	var sh remoteShell = posixShell{}
	out, err := sess.ssh(ctx, "uname -s", nil)
	if classifyHostKeyError(err) != hostKeyOK {
		return fmt.Errorf("ssh %s: %w", target, err)
	}
	if err != nil || isWindowsUname(strings.ToLower(strings.TrimSpace(out))) {
		probe, perr := sess.ssh(ctx, encodePS(windowsProbeScript), nil)
		if _, ok := parseWindowsProbe(probe); perr != nil || !ok {
			if err != nil {
				return fmt.Errorf("ssh %s: %w", target, err)
			}
			return fmt.Errorf("ssh %s: unable to detect the remote OS", target)
		}
		sh = windowsShell{}
	}
	findOut, err := sess.ssh(ctx, sh.findKonsertd(), nil)
	if err != nil {
		return fmt.Errorf("look for konsertd on %s: %w", target, err)
	}
	exe, found, _ := parseFind(findOut)
	if !found {
		return nil
	}
	args := []string{"hub-key", "remove", "--hub", hubID}
	if workerID != "" {
		args = append(args, "--worker", workerID)
	}
	if _, err := sess.ssh(ctx, sh.konsertd(exe, args, false), nil); err != nil {
		return fmt.Errorf("konsertd hub-key remove on %s: %w", target, err)
	}
	return nil
}
