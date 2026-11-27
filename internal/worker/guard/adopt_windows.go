//go:build windows

package guard

import (
	"errors"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Some children are created outside their parent's Job Object. Seen live: the
// Microsoft Store build of PowerShell 7 (the shell Claude Code uses on Windows) is
// inside the job, but every process it starts is not, so a memory hog run from an
// agent's shell survived TerminateJobObject. Their parent PID still names the
// process that started them, so the guard walks descendants of the session's
// processes and adopts any that escaped: into the job when Windows allows it,
// otherwise held by handle and counted, frozen and killed alongside the job.

const (
	adoptEvery = time.Second // background scan rate (forced scans run on shed, kill and release)
	knownCap   = 4096        // processes remembered per session
	adoptRight = windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.PROCESS_SET_QUOTA |
		windows.PROCESS_TERMINATE | windows.PROCESS_SUSPEND_RESUME
)

var procNtSuspendProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtSuspendProcess")

// creationTime is a process's creation FILETIME as a number (0 unknown).
func creationTime(h windows.Handle) uint64 {
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return 0
	}
	return uint64(c.HighDateTime)<<32 | uint64(c.LowDateTime)
}

func pidCreationTime(pid uint32) uint64 {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	return creationTime(h)
}

// childrenByParent maps each parent PID to the processes that name it, from one
// process snapshot.
func childrenByParent() (map[uint32][]uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	out := map[uint32][]uint32{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID != e.ParentProcessID && e.ProcessID != 0 {
			out[e.ParentProcessID] = append(out[e.ParentProcessID], e.ProcessID)
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return out, nil
}

// adopt brings escaped descendants of the session's processes under the session.
// Unforced calls scan at most once per adoptEvery.
func (ws *winSess) adopt(force bool) {
	if ws == nil {
		return
	}
	ws.adoptMu.Lock()
	defer ws.adoptMu.Unlock()
	ws.mu.Lock()
	job := ws.job
	if job == 0 || (!force && time.Since(ws.lastAdopt) < adoptEvery) {
		ws.mu.Unlock()
		return
	}
	ws.lastAdopt = time.Now()
	ws.mu.Unlock()

	// Remember every job member with its creation time: a dead member's PID can still
	// be the parent of an escaped child.
	for _, pid := range jobPIDs(job) {
		if _, ok := ws.known[pid]; !ok && len(ws.known) < knownCap {
			if ct := pidCreationTime(pid); ct != 0 {
				ws.known[pid] = ct
			}
		}
	}
	kids, err := childrenByParent()
	if err != nil {
		return
	}
	queue := make([]uint32, 0, len(ws.known))
	for pid := range ws.known {
		queue = append(queue, pid)
	}
	for i := 0; i < len(queue); i++ {
		parent := queue[i]
		parentCT := ws.known[parent]
		// If the parent's PID now belongs to a different process, only children
		// created before that process can be ours.
		reusedAt := uint64(0)
		if now := pidCreationTime(parent); now != 0 && now != parentCT {
			reusedAt = now
		}
		for _, c := range kids[parent] {
			if _, ok := ws.known[c]; ok {
				continue
			}
			if len(ws.known) >= knownCap {
				return
			}
			h, err := windows.OpenProcess(adoptRight, false, c)
			if err != nil {
				continue
			}
			ct := creationTime(h)
			if ct == 0 || ct < parentCT || (reusedAt != 0 && ct >= reusedAt) {
				_ = windows.CloseHandle(h)
				continue
			}
			ws.known[c] = ct
			queue = append(queue, c)
			var in int32
			if r, _, _ := procIsProcessInJob.Call(uintptr(h), uintptr(job), uintptr(unsafe.Pointer(&in))); r != 0 && in != 0 {
				_ = windows.CloseHandle(h)
				continue
			}
			if windows.AssignProcessToJobObject(job, h) == nil {
				_ = windows.CloseHandle(h)
				continue
			}
			ws.mu.Lock()
			ws.extras[c] = h
			ws.mu.Unlock()
		}
	}
}

// extraHandles returns the handles of adopted processes held outside the job.
func (ws *winSess) extraHandles() []windows.Handle {
	if ws == nil {
		return nil
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	out := make([]windows.Handle, 0, len(ws.extras))
	for _, h := range ws.extras {
		out = append(out, h)
	}
	return out
}

func alive(h windows.Handle) bool {
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}

// suspendAll suspends every process of the session (job members and adopted ones).
func (ws *winSess) suspendAll(job windows.Handle) int {
	n := 0
	for _, pid := range jobPIDs(job) {
		if h, err := windows.OpenProcess(windows.PROCESS_SUSPEND_RESUME, false, pid); err == nil {
			if r, _, _ := procNtSuspendProcess.Call(uintptr(h)); r == 0 {
				n++
			}
			_ = windows.CloseHandle(h)
		}
	}
	for _, h := range ws.extraHandles() {
		if alive(h) {
			if r, _, _ := procNtSuspendProcess.Call(uintptr(h)); r == 0 {
				n++
			}
		}
	}
	return n
}

// killExtras terminates adopted processes held outside the job.
func (ws *winSess) killExtras() int {
	n := 0
	for _, h := range ws.extraHandles() {
		if alive(h) && windows.TerminateProcess(h, 1) == nil {
			n++
		}
	}
	return n
}
