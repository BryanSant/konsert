//go:build windows

package shells

import (
	"errors"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// toolhelpSource reads the process table with a Toolhelp snapshot (pid, parent, image
// name). Start time, CPU and the command line need a process handle, so they are read
// only for processes in a session's tree (Details, Argv). Windows has no process groups.
type toolhelpSource struct{}

// OSSource returns the process table of this machine.
func OSSource() Source { return toolhelpSource{} }

func (toolhelpSource) Snapshot() ([]Proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var out []Proc
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == 0 {
			continue
		}
		out = append(out, Proc{PID: int(e.ProcessID), PPID: int(e.ParentProcessID), Name: windows.UTF16ToString(e.ExeFile[:])})
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return out, nil
}

func (toolhelpSource) Details(p *Proc) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(p.PID))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return
	}
	p.Start = time.Unix(0, created.Nanoseconds())
	ft := func(f windows.Filetime) time.Duration {
		return time.Duration(int64(f.HighDateTime)<<32|int64(f.LowDateTime)) * 100
	}
	p.CPU, p.CPUKnown = ft(kernel)+ft(user), true
}

// Argv reads the command line with NtQueryInformationProcess
// (ProcessCommandLineInformation, Windows 8.1+) and splits it the way the C runtime does.
func (toolhelpSource) Argv(pid int) []string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(h)
	buf := make([]byte, 4096)
	for range 3 {
		var n uint32
		err = windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, unsafe.Pointer(&buf[0]), uint32(len(buf)), &n)
		if err == nil {
			break
		}
		if int(n) <= len(buf) {
			return nil
		}
		buf = make([]byte, n)
	}
	if err != nil {
		return nil
	}
	us := (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0]))
	if us.Buffer == nil || us.Length == 0 {
		return nil
	}
	line := windows.UTF16ToString(unsafe.Slice(us.Buffer, us.Length/2))
	argv, err := windows.DecomposeCommandLine(line)
	if err != nil {
		return []string{line}
	}
	return argv
}
