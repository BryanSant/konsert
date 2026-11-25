package sshutil

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrUnreachable marks an uninstall that could not reach the host (try again later),
// as opposed to one the host refused or that failed there.
var ErrUnreachable = errors.New("host unreachable")

// UninstallRemote removes syred identity workerID from target over the user's own
// SSH access (the hub key itself may only attach), when the hub removes the machine:
// `syred uninstall --worker <id> --hub <hub id>` removes its service (so nothing
// restarts it), its config, credential and state, hubID's key grant for it, and the
// agent hooks if no other syred remains on the account. It returns what the remote
// uninstall printed. A host without syred has nothing to remove.
func UninstallRemote(parent context.Context, target, hubID, workerID string) (string, error) {
	t, err := ParseTarget(target)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	sess := &session{t: t}
	var sh remoteShell = posixShell{}
	out, err := sess.ssh(ctx, "uname -s", nil)
	if classifyHostKeyError(err) != hostKeyOK {
		return "", fmt.Errorf("ssh %s: %w", target, err)
	}
	if err != nil || isWindowsUname(strings.ToLower(strings.TrimSpace(out))) {
		probe, perr := sess.ssh(ctx, encodePS(windowsProbeScript), nil)
		if _, ok := parseWindowsProbe(probe); perr != nil || !ok {
			if err != nil {
				return "", fmt.Errorf("ssh %s: %w: %w", target, ErrUnreachable, err)
			}
			return "", fmt.Errorf("ssh %s: unable to detect the remote OS", target)
		}
		sh = windowsShell{}
	}
	findOut, err := sess.ssh(ctx, sh.findSyred(), nil)
	if err != nil {
		return "", fmt.Errorf("look for syred on %s: %w", target, err)
	}
	exe, found, _ := parseFind(findOut)
	if !found {
		return "syred is not installed on " + target, nil
	}
	args := []string{"uninstall", "--worker", workerID}
	if hubID != "" {
		args = append(args, "--hub", hubID)
	}
	res, err := sess.ssh(ctx, sh.syred(exe, args, false), nil)
	res = strings.TrimSpace(res)
	if err != nil {
		return res, fmt.Errorf("syred uninstall on %s: %w", target, err)
	}
	return res, nil
}
