//go:build windows

package elevate

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
)

// ErrDeclined means the user declined the UAC prompt.
var ErrDeclined = errors.New("the administrator prompt was declined")

var (
	modshell32          = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteExW = modshell32.NewProc("ShellExecuteExW")

	moduser32                     = windows.NewLazySystemDLL("user32.dll")
	procGetProcessWindowStation   = moduser32.NewProc("GetProcessWindowStation")
	procGetUserObjectInformationW = moduser32.NewProc("GetUserObjectInformationW")

	modkernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleWindow = modkernel32.NewProc("GetConsoleWindow")
)

// Detect describes this process for Decide.
func Detect(noElevate bool) Situation {
	p := probe()
	return Situation{
		Elevated:    windows.GetCurrentProcessToken().IsElevated(),
		Interactive: Interactive(p),
		SSH:         p.SSH,
		NoElevate:   noElevate,
		Sudo:        sudoMode(),
	}
}

func probe() Probe {
	var p Probe
	for _, k := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		if os.Getenv(k) != "" {
			p.SSH = true
		}
	}
	p.CI = os.Getenv("CI") != ""
	p.Service, _ = svc.IsWindowsService()
	var sid uint32
	if windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &sid) == nil {
		p.SessionID = sid
	}
	p.VisibleDesktop = visibleWindowStation()
	if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd != 0 {
		p.Console = true
	}
	return p
}

// visibleWindowStation reports WSF_VISIBLE on the process's window station.
func visibleWindowStation() bool {
	const uoiFlags, wsfVisible = 1, 1
	ws, _, _ := procGetProcessWindowStation.Call()
	if ws == 0 {
		return false
	}
	var flags struct {
		Inherit  int32
		Reserved int32
		Flags    uint32
	}
	var needed uint32
	r, _, _ := procGetUserObjectInformationW.Call(ws, uoiFlags, uintptr(unsafe.Pointer(&flags)), unsafe.Sizeof(flags), uintptr(unsafe.Pointer(&needed)))
	return r != 0 && flags.Flags&wsfVisible != 0
}

// sudoMode reads the sudo setting and policy (see SudoMode).
func sudoMode() SudoMode {
	read := func(path string) (uint32, bool) {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
		if err != nil {
			return 0, false
		}
		defer func() { _ = k.Close() }()
		v, _, err := k.GetIntegerValue("Enabled")
		if err != nil {
			return 0, false
		}
		return uint32(v), true
	}
	setting, haveSetting := read(`SOFTWARE\Microsoft\Windows\CurrentVersion\Sudo`)
	policy, havePolicy := read(`SOFTWARE\Policies\Microsoft\Windows\Sudo`)
	return SudoModeFrom(setting, haveSetting, policy, havePolicy)
}

func isBrokenPipe(err error) bool {
	return errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) || errors.Is(err, windows.ERROR_NO_DATA)
}

// pipePrefix starts every elevation pipe name; the rest is 128 random bits in hex.
const pipePrefix = `\\.\pipe\konsertd-elevate-`

var pipeNameRE = regexp.MustCompile(`^\\\\\.\\pipe\\konsertd-elevate-[0-9a-f]{32}$`)

func newPipeName() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return pipePrefix + hex.EncodeToString(b[:]), nil
}

// pipeSDDL is the pipe's DACL: generic all for the user's own SID only (protected, so
// nothing is inherited). Nobody else, not even SYSTEM or Administrators, gets an ACE.
// The elevated child carries the same user SID (a UAC consent prompt elevates the same
// account), so it can open the pipe.
func pipeSDDL(userSID string) string { return "D:P(A;;GA;;;" + userSID + ")" }

// launcher starts the child; tests replace verb and file.
type launcher struct {
	verb string // "runas" for UAC; "" (open) in tests
	file string // the konsertd executable
}

// Run relaunches this executable elevated (UAC prompt), hands it req over a private
// pipe and relays its output to stdout and stderr. It returns the child's exit code.
func Run(req Request, stdout, stderr io.Writer) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	return launcher{verb: "runas", file: exe}.run(req, stdout, stderr)
}

func (l launcher) run(req Request, stdout, stderr io.Writer) (int, error) {
	name, err := newPipeName()
	if err != nil {
		return 0, err
	}
	pipe, err := createPipe(name)
	if err != nil {
		return 0, err
	}
	defer func() { _ = windows.CloseHandle(pipe) }()

	proc, err := l.shellExecute(ChildCommand+" "+name, req.Dir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = windows.CloseHandle(proc) }()

	if err := connectChild(pipe, proc, 2*time.Minute); err != nil {
		code, _ := waitExit(proc, 10*time.Second)
		return code, err
	}
	// From here the pipe belongs to the os.File (overlapped I/O through Go's poller).
	f := os.NewFile(uintptr(pipe), name)
	pipe = windows.InvalidHandle
	defer func() { _ = f.Close() }()
	code, rerr := Relay(f, req, stdout, stderr)
	pcode, werr := waitExit(proc, 30*time.Second)
	if errors.Is(rerr, ErrNoExitCode) && werr == nil {
		return pcode, fmt.Errorf("%w (exit code %d)", ErrNoExitCode, pcode)
	}
	return code, rerr
}

// createPipe creates the single instance of a duplex byte pipe that only this user's
// SID may open, refusing remote clients and failing if the name already exists.
func createPipe(name string) (windows.Handle, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return windows.InvalidHandle, err
	}
	sd, err := windows.SecurityDescriptorFromString(pipeSDDL(tu.User.Sid.String()))
	if err != nil {
		return windows.InvalidHandle, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	h, err := windows.CreateNamedPipe(windows.StringToUTF16Ptr(name),
		windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		1, 64<<10, 64<<10, 0, sa)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("create the elevation pipe: %w", err)
	}
	return h, nil
}

// connectChild waits for a client on pipe and checks it is the process proc: another
// process of this user that opened the pipe first would be refused. It gives up when
// proc exits first or after timeout.
func connectChild(pipe, proc windows.Handle, timeout time.Duration) error {
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(ev) }()
	ov := windows.Overlapped{HEvent: ev}
	err = windows.ConnectNamedPipe(pipe, &ov)
	switch {
	case err == nil, errors.Is(err, windows.ERROR_PIPE_CONNECTED):
	case errors.Is(err, windows.ERROR_IO_PENDING):
		which, werr := windows.WaitForMultipleObjects([]windows.Handle{ev, proc}, false, uint32(timeout/time.Millisecond))
		switch {
		case werr != nil:
			_ = windows.CancelIoEx(pipe, &ov)
			return werr
		case which == windows.WAIT_OBJECT_0:
			var n uint32
			if err := windows.GetOverlappedResult(pipe, &ov, &n, false); err != nil {
				return fmt.Errorf("wait for the elevated process: %w", err)
			}
		case which == windows.WAIT_OBJECT_0+1:
			_ = windows.CancelIoEx(pipe, &ov)
			var n uint32
			_ = windows.GetOverlappedResult(pipe, &ov, &n, true)
			return errors.New("the elevated process exited before connecting (if another account approved the prompt, run the command from that account's elevated terminal instead)")
		default:
			_ = windows.CancelIoEx(pipe, &ov)
			var n uint32
			_ = windows.GetOverlappedResult(pipe, &ov, &n, true)
			return errors.New("timed out waiting for the elevated process")
		}
	default:
		return fmt.Errorf("wait for the elevated process: %w", err)
	}
	var client uint32
	if err := windows.GetNamedPipeClientProcessId(pipe, &client); err != nil {
		return fmt.Errorf("identify the pipe client: %w", err)
	}
	want, err := windows.GetProcessId(proc)
	if err != nil {
		return err
	}
	if client != want {
		_ = windows.DisconnectNamedPipe(pipe)
		return fmt.Errorf("the elevation pipe was opened by process %d, not the elevated konsertd (%d); refusing", client, want)
	}
	return nil
}

func waitExit(proc windows.Handle, timeout time.Duration) (int, error) {
	ev, err := windows.WaitForSingleObject(proc, uint32(timeout/time.Millisecond))
	if err != nil {
		return 0, err
	}
	if ev != windows.WAIT_OBJECT_0 {
		return 0, errors.New("the elevated process did not exit")
	}
	var code uint32
	if err := windows.GetExitCodeProcess(proc, &code); err != nil {
		return 0, err
	}
	return int(code), nil
}

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	cbSize         uint32
	fMask          uint32
	hwnd           windows.Handle
	lpVerb         *uint16
	lpFile         *uint16
	lpParameters   *uint16
	lpDirectory    *uint16
	nShow          int32
	hInstApp       windows.Handle
	lpIDList       uintptr
	lpClass        *uint16
	hkeyClass      windows.Handle
	dwHotKey       uint32
	hIconOrMonitor windows.Handle
	hProcess       windows.Handle
}

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	seeMaskFlagNoUI       = 0x00000400
	swHide                = 0
)

// shellExecute starts l.file with params (and the UAC prompt for "runas"), its console
// window hidden: the output comes back through the pipe. It returns the process handle.
func (l launcher) shellExecute(params, dir string) (windows.Handle, error) {
	// ShellExecuteEx wants COM initialized (single-threaded apartment) on its thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE); err == nil {
		defer windows.CoUninitialize()
	}
	info := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync | seeMaskFlagNoUI,
		lpFile:       windows.StringToUTF16Ptr(l.file),
		lpParameters: windows.StringToUTF16Ptr(params),
		nShow:        swHide,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))
	if l.verb != "" {
		info.lpVerb = windows.StringToUTF16Ptr(l.verb)
	}
	if dir != "" {
		info.lpDirectory = windows.StringToUTF16Ptr(dir)
	}
	r, _, callErr := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		if errors.Is(callErr, windows.ERROR_CANCELLED) {
			return 0, ErrDeclined
		}
		return 0, fmt.Errorf("start the elevated konsertd: %w", callErr)
	}
	if info.hProcess == 0 {
		return 0, errors.New("start the elevated konsertd: no process handle")
	}
	return info.hProcess, nil
}

// OpenChildPipe is the elevated child's end: it opens the parent's pipe, which must be
// a konsertd elevation pipe on this machine.
func OpenChildPipe(name string) (io.ReadWriteCloser, error) {
	if !pipeNameRE.MatchString(strings.ToLower(name)) || name != strings.ToLower(name) {
		return nil, fmt.Errorf("%q is not a konsertd elevation pipe", name)
	}
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(name),
		windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
	if err != nil {
		return nil, fmt.Errorf("open the elevation pipe: %w", err)
	}
	return os.NewFile(uintptr(h), name), nil
}
