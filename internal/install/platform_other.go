//go:build !darwin && !linux && !windows

package install

import (
	"fmt"
	"runtime"
)

// DefaultEnv is unsupported on this OS.
func DefaultEnv() (Env, error) {
	return Env{}, fmt.Errorf("konsertd install is not supported on %s", runtime.GOOS)
}

func defaultBackend() (backend, error) {
	return nil, fmt.Errorf("konsertd install is not supported on %s; start `konsertd run` with your own supervisor", runtime.GOOS)
}
