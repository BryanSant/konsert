//go:build windows

package proctree

import (
	"os/exec"
	"testing"
)

// startTicker runs cmd.exe, which starts a PowerShell child that writes an increasing
// counter to path: suspension must reach the descendant.
func startTicker(t *testing.T, path string) (int, func()) {
	t.Helper()
	script := `$i=0; while ($true) { $i++; [IO.File]::WriteAllText('` + path + `', "$i"); Start-Sleep -Milliseconds 50 }`
	cmd := exec.Command("cmd.exe", "/c", "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	return pid, func() {
		_ = Resume(pid)
		kill := exec.Command("taskkill", "/F", "/T", "/PID", itoa(pid))
		_ = kill.Run()
		_ = cmd.Wait()
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
