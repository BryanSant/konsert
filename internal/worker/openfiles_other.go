//go:build !linux

package worker

// processOpenFiles is nil where listing a process's open files is costly: sessions are
// matched to processes by cwd instead.
var processOpenFiles func(pid int) []string
