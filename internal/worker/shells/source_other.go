//go:build !linux && !darwin && !windows

package shells

import "errors"

type noSource struct{}

// OSSource returns a source that reports no processes: shell counting is implemented
// for Linux, macOS and Windows.
func OSSource() Source { return noSource{} }

func (noSource) Snapshot() ([]Proc, error) {
	return nil, errors.New("process table not supported on this OS")
}

func (noSource) Details(*Proc) {}

func (noSource) Argv(int) []string { return nil }
