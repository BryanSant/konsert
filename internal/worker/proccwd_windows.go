//go:build windows

package worker

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processCwd reads another process's current directory from its PEB: the process
// parameters' CurrentDirectory. It needs query and VM-read access, which a user has to
// their own processes; "" when denied or on any error.
func processCwd(pid int) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var pbi windows.PROCESS_BASIC_INFORMATION
	if windows.NtQueryInformationProcess(h, windows.ProcessBasicInformation, unsafe.Pointer(&pbi), uint32(unsafe.Sizeof(pbi)), nil) != nil || pbi.PebBaseAddress == nil {
		return ""
	}
	read := func(addr uintptr, dst unsafe.Pointer, n uintptr) bool {
		return windows.ReadProcessMemory(h, addr, (*byte)(dst), n, nil) == nil
	}
	var peb windows.PEB
	if !read(uintptr(unsafe.Pointer(pbi.PebBaseAddress)), unsafe.Pointer(&peb), unsafe.Sizeof(peb)) || peb.ProcessParameters == nil {
		return ""
	}
	var params windows.RTL_USER_PROCESS_PARAMETERS
	if !read(uintptr(unsafe.Pointer(peb.ProcessParameters)), unsafe.Pointer(&params), unsafe.Sizeof(params)) {
		return ""
	}
	dos := params.CurrentDirectory.DosPath
	if dos.Buffer == nil || dos.Length == 0 || dos.Length > 32767*2 {
		return ""
	}
	buf := make([]uint16, dos.Length/2)
	if !read(uintptr(unsafe.Pointer(dos.Buffer)), unsafe.Pointer(&buf[0]), uintptr(dos.Length)) {
		return ""
	}
	cwd := windows.UTF16ToString(buf)
	if len(cwd) > 3 { // keep the root's "C:\"
		cwd = strings.TrimRight(cwd, `\`)
	}
	return cwd
}
