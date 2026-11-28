package realname

import (
	"context"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	secur32         = windows.NewLazySystemDLL("secur32.dll")
	procGetUserName = secur32.NewProc("GetUserNameExW")
)

// nameDisplay is EXTENDED_NAME_FORMAT NameDisplay.
const nameDisplay = 3

// osFullName is the signed-in user's display name (also for Microsoft and domain
// accounts). It describes the current user only, which is who runs the hub.
func osFullName(_ context.Context, _ string) string {
	n := uint32(256)
	buf := make([]uint16, n)
	r, _, _ := procGetUserName.Call(nameDisplay, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}
