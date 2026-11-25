//go:build windows

package worker

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

// Execution state flags for SetThreadExecutionState (winbase.h).
const (
	esSystemRequired = 0x00000001
	esContinuous     = 0x80000000
)

var procSetThreadExecutionState = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadExecutionState")

// setThreadExecutionState returns the previous state, or 0 and the Win32 error on failure.
func setThreadExecutionState(flags uint32) (uint32, error) {
	if err := procSetThreadExecutionState.Find(); err != nil {
		return 0, err
	}
	r, _, callErr := procSetThreadExecutionState.Call(uintptr(flags))
	// A zero result means failure only when Windows also set a last error: the previous
	// state of a thread that never set one can legitimately be zero.
	if r == 0 {
		var errno syscall.Errno
		if errors.As(callErr, &errno) && errno != 0 {
			return 0, errno
		}
	}
	return uint32(r), nil
}

// defaultWakeBackend on Windows uses SetThreadExecutionState(ES_CONTINUOUS |
// ES_SYSTEM_REQUIRED). The state belongs to the calling thread, so it is set and cleared
// on one goroutine locked to its OS thread for the lifetime of the lock. Windows drops the
// request automatically if syred exits.
func defaultWakeBackend() wakeBackend {
	return &execStateWakeBackend{setState: setThreadExecutionState}
}

type execStateWakeBackend struct {
	// setState wraps SetThreadExecutionState; injectable for tests.
	setState func(flags uint32) (uint32, error)
}

func (b *execStateWakeBackend) Method() string { return "SetThreadExecutionState" }

func (b *execStateWakeBackend) Acquire(string) (wakeHolder, error) {
	h := &execStateHolder{release: make(chan struct{}), done: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer close(h.done)
		if _, err := b.setState(esContinuous | esSystemRequired); err != nil {
			err = fmt.Errorf("SetThreadExecutionState failed: %w", err)
			// The thread is untouched; let the runtime keep using it.
			runtime.UnlockOSThread()
			started <- err
			return
		}
		started <- nil
		<-h.release
		_, _ = b.setState(esContinuous)
		runtime.UnlockOSThread()
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return h, nil
}

type execStateHolder struct {
	once    sync.Once
	release chan struct{}
	done    chan struct{}
}

// Release clears the execution state on the owning thread and waits for it.
func (h *execStateHolder) Release() error {
	h.once.Do(func() { close(h.release) })
	<-h.done
	return nil
}

// Done is closed only after Release: the thread-held state cannot end on its own.
func (h *execStateHolder) Done() <-chan struct{} { return h.done }

func (h *execStateHolder) Err() error { return errors.New("execution state released") }
