//go:build !linux && !darwin && !windows

package procs

import "errors"

func osTable() ([]Proc, error) {
	return nil, errors.New("procs: process table not supported on this OS")
}
