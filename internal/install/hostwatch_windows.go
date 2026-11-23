//go:build windows

package install

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// StopWithHeadlessHost returns a context that is canceled when the `conhost --headless`
// hosting this process exits. The logon task runs konsertd under such a conhost; when
// Task Scheduler ends the task (uninstall, reinstall, `schtasks /End`) it terminates the
// conhost only, and konsertd would otherwise keep running with no console. Watching the
// parent turns that into a clean shutdown (sessions stopped, wake lock released).
//
// It does nothing unless the parent process is conhost.exe, so konsertd started from a
// terminal or shell is unaffected.
func StopWithHeadlessHost(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	ppid := os.Getppid()
	if ppid <= 0 {
		return ctx, cancel
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(ppid))
	if err != nil {
		return ctx, cancel
	}
	if !strings.EqualFold(processImageBase(h), "conhost.exe") {
		_ = windows.CloseHandle(h)
		return ctx, cancel
	}
	// One goroutine for the life of the process; the handle is held until conhost exits.
	go func() {
		_, _ = windows.WaitForSingleObject(h, windows.INFINITE)
		_ = windows.CloseHandle(h)
		cancel()
	}()
	return ctx, cancel
}

func processImageBase(h windows.Handle) string {
	buf := make([]uint16, windows.MAX_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:n]))
}
