package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/install"
	"github.com/google/uuid"
)

// The local socket (SYR-K1B60). A syred without a controller URL serves the hub on
// an AF_UNIX socket in its state directory (every OS, Windows 10 1803+ included):
// <stateDir>/run/<worker id>.sock, in a directory only the user can open (0700, or a
// protected user-only DACL on Windows). The hub reaches it directly when it runs on
// the same machine, and over SSH otherwise: the hub key's forced command is
// `syred attach`, which bridges the SSH session's stdin/stdout to the socket.
// Keying the path by identity lets daemons sharing a home directory (a container and
// its host) each have their own.

// maxSocketPath is the shortest sun_path limit of the supported platforms (macOS: 104
// bytes including the terminating NUL).
const maxSocketPath = 103

// LocalSocketPath is where the daemon with workerID listens ("" if there is no state
// directory or the id is not a UUID).
func LocalSocketPath(stateDir, workerID string) string {
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	if stateDir == "" {
		return ""
	}
	if u, err := uuid.Parse(workerID); err != nil || u.String() != workerID {
		return ""
	}
	return filepath.Join(stateDir, "run", workerID+".sock")
}

// listenLocal listens on path, replacing a stale socket file left by a daemon that did
// not exit cleanly but refusing to take over one another live daemon still serves.
func listenLocal(path string) (net.Listener, error) {
	if path == "" {
		return nil, errors.New("no local socket path (no state directory or worker id)")
	}
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("socket path %s is too long (max %d bytes); set %s to a shorter directory", path, maxSocketPath, StateDirEnv)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := install.RestrictDir(dir); err != nil {
		return nil, fmt.Errorf("restrict %s: %w", dir, err)
	}
	if _, err := os.Lstat(path); err == nil {
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			_ = c.Close()
			return nil, fmt.Errorf("another syred is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(true)
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o600)
	}
	return ln, nil
}

// AttachRequest picks the identity `syred attach` connects to: the worker id the
// hub asked for (the last word of SSH_ORIGINAL_COMMAND, or an argument), which must be
// in allow when the forced command restricts it; with no request, the only allowed id.
func AttachRequest(requested string, allow []string) (string, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if f := strings.Fields(requested); len(f) > 0 {
		requested = f[len(f)-1]
	}
	if requested != "" {
		if u, err := uuid.Parse(requested); err != nil || u.String() != requested {
			requested = ""
		}
	}
	switch {
	case requested != "" && (len(allow) == 0 || containsFold(allow, requested)):
		return requested, nil
	case requested != "":
		return "", fmt.Errorf("worker %s is not allowed for this key", requested)
	case len(allow) == 1:
		return strings.ToLower(allow[0]), nil
	default:
		return "", errors.New("no worker id requested")
	}
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), s) {
			return true
		}
	}
	return false
}

// Attach bridges in/out (the SSH session) to the daemon's socket at path until either
// side closes.
func Attach(ctx context.Context, path string, in io.Reader, out io.Writer) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("syred is not running here (no socket at %s): %w", path, err)
	}
	defer conn.Close()
	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(conn, in)
		// The hub hung up: tell the daemon no more is coming, and let its reply drain.
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
		done <- err
	}()
	go func() {
		_, err := io.Copy(out, conn)
		done <- err
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		// Either direction ending ends the session: a hub that went away never comes
		// back on this stream, and a daemon that closed has nothing more to say.
		return err
	}
}
