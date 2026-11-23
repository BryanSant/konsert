//go:build windows

package install

import (
	"context"
	"time"

	"golang.org/x/sys/windows/svc"
)

// serviceStopTimeout bounds how long Stop/Shutdown waits for the daemon to end its
// sessions and release the wake lock before reporting Stopped anyway.
const serviceStopTimeout = 25 * time.Second

// RunAsServiceIfNeeded runs run under the Windows service control manager when this
// process was started by it. It reports handled=false (and does nothing) otherwise, so
// the caller runs the daemon normally. Under the SCM, Stop and Shutdown cancel run's
// context, which makes the daemon stop its agent sessions and release the wake lock.
func RunAsServiceIfNeeded(run func(context.Context) error) (handled bool, err error) {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return false, nil
	}
	h := &serviceHandler{run: run}
	if err := svc.Run(BaseName, h); err != nil {
		return true, err
	}
	return true, h.err
}

type serviceHandler struct {
	run func(context.Context) error
	err error
}

// Execute implements svc.Handler.
func (h *serviceHandler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case err := <-done:
			// The daemon ended on its own (removed from the fleet, or no token). A
			// non-zero exit lets the recovery actions restart it after an error.
			h.err = err
			if err != nil && err != context.Canceled {
				return true, 1
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32((serviceStopTimeout + 5*time.Second) / time.Millisecond)}
				cancel()
				select {
				case <-done:
				case <-time.After(serviceStopTimeout):
				}
				return false, 0
			}
		}
	}
}
