//go:build windows

package procs

import (
	"errors"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// filetimeEpoch is 1601-01-01 in 100 ns units before the Unix epoch.
const filetimeEpoch = 116444736000000000

// bufPool keeps the NtQuerySystemInformation buffer between samples (it is large: a few
// hundred KB with a few hundred processes).
var bufPool = sync.Pool{New: func() any { b := make([]byte, 512<<10); return &b }}

func osTable() ([]Proc, error) {
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	for range 8 {
		var n uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&buf[0]), uint32(len(buf)), &n)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			return nil, err
		}
		// Grow past what it asked for: processes may start before the next call.
		buf = make([]byte, max(int(n)+64<<10, 2*len(buf)))
		*bp = buf
	}
	var out []Proc
	for off := 0; off < len(buf); {
		e := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buf[off]))
		if pid := int(e.UniqueProcessID); pid != 0 { // 0 is the idle process
			p := Proc{
				PID:  pid,
				PPID: int(e.InheritedFromUniqueProcessID),
				CPU:  time.Duration(e.UserTime+e.KernelTime) * 100,
				RSS:  uint64(e.WorkingSetSize),
			}
			if e.ImageName.Length > 0 && e.ImageName.Buffer != nil {
				p.Name = filepath.Base(windows.UTF16ToString(unsafe.Slice(e.ImageName.Buffer, e.ImageName.Length/2)))
			} else if pid == 4 {
				p.Name = "System"
			}
			if e.CreateTime > filetimeEpoch {
				p.Start = time.Unix(0, (e.CreateTime-filetimeEpoch)*100)
			}
			out = append(out, p)
		}
		if e.NextEntryOffset == 0 {
			break
		}
		off += int(e.NextEntryOffset)
	}
	return out, nil
}
