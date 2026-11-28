//go:build !windows

package worker

// processCwd is Windows-only; other systems read the cwd in resolveProcessCwd.
func processCwd(int) string { return "" }
