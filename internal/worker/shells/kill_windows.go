//go:build windows

package shells

import "golang.org/x/sys/windows"

type terminateKiller struct{}

// OSKiller terminates processes with TerminateProcess (Windows has no gentle signal for
// console processes that syred does not own, so Terminate and Kill are the same).
func OSKiller() Killer { return terminateKiller{} }

func (terminateKiller) Terminate(pid int) error { return terminate(pid) }

func (terminateKiller) Kill(pid int) error { return terminate(pid) }

func terminate(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}
