package worker

import (
	"context"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetLastInputInfo = windows.NewLazySystemDLL("user32.dll").NewProc("GetLastInputInfo")
	procGetTickCount     = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetTickCount")
)

type lastInputInfo struct {
	size uint32
	time uint32
}

// platformUserIdle is GetLastInputInfo: the input of the desktop session syred runs
// in (a syred started as a service, in session 0, sees none, and reports nothing).
func platformUserIdle(context.Context) (time.Duration, string, bool) {
	if windows.WTSGetActiveConsoleSessionId() == 0xFFFFFFFF {
		return 0, "", false // no one is signed in at the console
	}
	var sid uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &sid); err != nil || sid == 0 {
		return 0, "", false
	}
	li := lastInputInfo{size: uint32(unsafe.Sizeof(lastInputInfo{}))}
	if r, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&li))); r == 0 {
		return 0, "", false
	}
	now, _, _ := procGetTickCount.Call()
	return time.Duration(uint32(now)-li.time) * time.Millisecond, "last_input", true
}
