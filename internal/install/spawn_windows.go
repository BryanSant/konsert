package install

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// spawnDetached starts name in a new process group, detached from this console, and
// does not wait for it.
func spawnDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// ScheduleDeleteOnExit spawns a detached cmd.exe that waits briefly for this process to
// exit and then deletes exePath (and emptyDir if empty).
func ScheduleDeleteOnExit(exePath, emptyDir string) error {
	cmdLine, err := BuildWindowsDeleteCmdLine(exePath, emptyDir)
	if err != nil {
		return err
	}
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
		CmdLine:       cmdLine,
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
