//go:build !windows

package proctree

import (
	"os/exec"
	"syscall"
	"testing"
)

// startTicker runs a shell (leading its own process group, like managed sessions) whose
// child loop writes an increasing counter to path.
func startTicker(t *testing.T, path string) (int, func()) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", `i=0; while :; do i=$((i+1)); echo $i > "$1"; sleep 0.05; done`, "sh", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	return pid, func() {
		_ = syscall.Kill(-pid, syscall.SIGCONT)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}
}
