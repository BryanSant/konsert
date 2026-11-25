//go:build windows

package worker

import (
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// otherSyredRunning reports whether another syred.exe process is currently running.
func otherSyredRunning(_ string) (bool, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, err
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snap, &pe); err != nil {
		return false, err
	}
	self := uint32(os.Getpid())
	parent := uint32(os.Getppid())
	for {
		if pe.ProcessID != self && pe.ProcessID != parent {
			if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), "syred.exe") {
				return true, nil
			}
		}
		if err := windows.Process32Next(snap, &pe); err != nil {
			break
		}
	}
	return false, nil
}
