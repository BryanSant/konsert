package worker

import "context"

// PtyStartOptions defines parameters for starting a cross-platform PTY process.
type PtyStartOptions struct {
	Binary string
	Args   []string
	Dir    string
	Env    []string
	Cols   uint16
	Rows   uint16
}

// PtyProcess abstracts a pseudo-terminal child process across Unix (PTY) and Windows (ConPTY).
type PtyProcess interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows uint16) error
	Interrupt() error
	Terminate() error
	Wait() (int, error)
	Close() error
}

// StartPty spawns a command inside a platform-native PTY/ConPTY.
func StartPty(ctx context.Context, opts PtyStartOptions) (PtyProcess, error) {
	if opts.Cols == 0 {
		opts.Cols = 120
	}
	if opts.Rows == 0 {
		opts.Rows = 36
	}
	return startPlatformPty(ctx, opts)
}
