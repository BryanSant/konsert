//go:build windows

package proctree

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ntdll            = windows.NewLazySystemDLL("ntdll.dll")
	procNtSuspendPro = ntdll.NewProc("NtSuspendProcess")
	procNtResumePro  = ntdll.NewProc("NtResumeProcess")
)

func suspend(pid int) error { return eachInTree(pid, procNtSuspendPro) }

func resume(pid int) error { return eachInTree(pid, procNtResumePro) }

// eachInTree applies an NtSuspendProcess-style call to pid and all its descendants.
func eachInTree(pid int, call *windows.LazyProc) error {
	if err := call.Find(); err != nil {
		return err
	}
	pids, err := tree(uint32(pid))
	if err != nil {
		return err
	}
	var firstErr error
	done := 0
	for _, p := range pids {
		h, err := windows.OpenProcess(windows.PROCESS_SUSPEND_RESUME, false, p)
		if err != nil {
			if firstErr == nil && p == uint32(pid) {
				firstErr = err
			}
			continue
		}
		r, _, _ := call.Call(uintptr(h))
		_ = windows.CloseHandle(h)
		if r != 0 && firstErr == nil {
			firstErr = fmt.Errorf("%s(%d): NTSTATUS 0x%x", call.Name, p, r)
			continue
		}
		done++
	}
	if done == 0 {
		if firstErr != nil {
			return firstErr
		}
		return ErrNoProcess
	}
	return nil
}

// tree returns root and every descendant process id (parents before children).
func tree(root uint32) ([]uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	children := map[uint32][]uint32{}
	found := false
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == root {
			found = true
		}
		if e.ProcessID != e.ParentProcessID {
			children[e.ParentProcessID] = append(children[e.ParentProcessID], e.ProcessID)
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	if !found {
		return nil, ErrNoProcess
	}
	out := []uint32{root}
	seen := map[uint32]bool{root: true}
	for i := 0; i < len(out); i++ {
		for _, c := range children[out[i]] {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out, nil
}
