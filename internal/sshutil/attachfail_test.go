package sshutil

import "testing"

// A failed attach says whether the host was unreachable (re-dial quietly) or reachable
// with its konsertd down (restart it).
func TestClassifyAttachFailure(t *testing.T) {
	for msg, want := range map[string]AttachFailure{
		"Error: konsertd is not running here (no socket at /home/me/.konsert/w.sock): dial unix /home/me/.konsert/w.sock: connect: no such file or directory": AttachDaemonDown,
		"sh: 1: /home/me/.local/bin/konsertd: not found":                                    AttachDaemonDown,
		"bash: line 1: /home/me/.local/bin/konsertd: No such file or directory":             AttachDaemonDown,
		"ssh: connect to host box port 22: Connection refused":                              AttachUnreachable,
		"ssh: connect to host box port 22: Operation timed out":                             AttachUnreachable,
		"ssh: Could not resolve hostname box: nodename nor servname provided, or not known": AttachUnreachable,
		"me@box: Permission denied (publickey).":                                            AttachUnreachable,
		"Host key verification failed.":                                                     AttachUnreachable,
		"Connection closed by 10.0.0.5 port 22":                                             AttachUnreachable,
		"failed to WebSocket dial: context deadline exceeded":                               AttachFailureOther,
		"": AttachFailureOther,
	} {
		if got := ClassifyAttachFailure(msg); got != want {
			t.Errorf("ClassifyAttachFailure(%q) = %s, want %s", msg, got, want)
		}
	}
}
