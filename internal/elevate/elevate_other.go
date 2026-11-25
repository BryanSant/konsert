//go:build !windows

package elevate

import (
	"errors"
	"io"
	"syscall"
)

// ErrDeclined means the user declined the UAC prompt (Windows only).
var ErrDeclined = errors.New("the administrator prompt was declined")

var errWindowsOnly = errors.New("elevation is only implemented on Windows")

// Detect describes this process: nothing to elevate outside Windows.
func Detect(noElevate bool) Situation { return Situation{NoElevate: noElevate} }

// Run is Windows-only.
func Run(Request, io.Writer, io.Writer) (int, error) { return 0, errWindowsOnly }

// OpenChildPipe is Windows-only.
func OpenChildPipe(string) (io.ReadWriteCloser, error) { return nil, errWindowsOnly }

func isBrokenPipe(err error) bool { return errors.Is(err, syscall.EPIPE) }
