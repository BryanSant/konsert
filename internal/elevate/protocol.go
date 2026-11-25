package elevate

import (
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"sync"
)

// The parent (unelevated) and the elevated child talk over one named pipe that only
// the parent can create and only the child it launched may use (see pipe_windows.go).
// Every message is a frame: one type byte, a big-endian uint32 payload length, the
// payload.
//
//	parent -> child  'R'  Request as JSON, exactly once, first: the command line,
//	                      working directory and (install) the account password
//	child  -> parent 'O'  bytes the command wrote to stdout
//	child  -> parent 'E'  bytes the command wrote to stderr
//	child  -> parent 'X'  exit code, a big-endian int32, last
//
// The password travels only in the request frame: never in argv, the environment or a
// file, and it is never logged.

// ChildCommand is the hidden konsertd subcommand the elevated process runs, with the
// pipe name as its only argument.
const ChildCommand = "elevated-child"

const (
	frameRequest = 'R'
	frameStdout  = 'O'
	frameStderr  = 'E'
	frameExit    = 'X'

	// maxFrame bounds a frame's payload; output is sent in chunks below it.
	maxFrame = 1 << 20
)

// Request is what the parent hands the elevated child.
type Request struct {
	// Args is the konsertd command line after the program name.
	Args []string `json:"args"`
	// Dir is the parent's working directory.
	Dir string `json:"dir"`
	// Password is the service account password (install --service), or "".
	Password string `json:"password,omitempty"`
}

// String never shows the password.
func (r Request) String() string {
	pw := ""
	if r.Password != "" {
		pw = " password=<redacted>"
	}
	return fmt.Sprintf("args=%q dir=%q%s", r.Args, r.Dir, pw)
}

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > maxFrame {
		return fmt.Errorf("frame of %d bytes exceeds %d", len(payload), maxFrame)
	}
	buf := make([]byte, 5+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:5])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes exceeds %d", n, maxFrame)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("short frame: %w", io.ErrUnexpectedEOF)
	}
	return hdr[0], payload, nil
}

// ErrNoExitCode means the child closed the pipe without reporting an exit code (it
// crashed or was killed); the caller falls back to the process exit code.
var ErrNoExitCode = errors.New("the elevated process ended without reporting a result")

// Relay is the parent's side: it sends req, then copies the child's output to stdout
// and stderr until the child reports its exit code.
func Relay(rw io.ReadWriter, req Request, stdout, stderr io.Writer) (int, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	err = writeFrame(rw, frameRequest, payload)
	clear(payload)
	if err != nil {
		return 0, fmt.Errorf("send the request to the elevated process: %w", err)
	}
	for {
		typ, data, err := readFrame(rw)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isBrokenPipe(err) {
				return 0, ErrNoExitCode
			}
			return 0, err
		}
		switch typ {
		case frameStdout:
			_, _ = stdout.Write(data)
		case frameStderr:
			_, _ = stderr.Write(data)
		case frameExit:
			if len(data) != 4 {
				return 0, fmt.Errorf("malformed exit frame (%d bytes)", len(data))
			}
			return int(int32(binary.BigEndian.Uint32(data))), nil
		default:
			return 0, fmt.Errorf("unexpected frame %q from the elevated process", typ)
		}
	}
}

// Child is the elevated side of the pipe after Accept.
type Child struct {
	mu sync.Mutex
	w  io.Writer
}

// Accept is the child's side: it reads the parent's request, which must come first.
func Accept(rw io.ReadWriter) (Request, *Child, error) {
	var req Request
	typ, data, err := readFrame(rw)
	if err != nil {
		return req, nil, fmt.Errorf("read the request: %w", err)
	}
	defer clear(data)
	if typ != frameRequest {
		return req, nil, fmt.Errorf("expected a request frame, got %q", typ)
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return req, nil, fmt.Errorf("parse the request: %w", err)
	}
	if len(req.Args) == 0 {
		return req, nil, errors.New("the request has no command")
	}
	return req, &Child{w: rw}, nil
}

// Stdout returns a writer that relays to the parent's stdout.
func (c *Child) Stdout() io.Writer { return streamWriter{c, frameStdout} }

// Stderr returns a writer that relays to the parent's stderr.
func (c *Child) Stderr() io.Writer { return streamWriter{c, frameStderr} }

// Exit reports the command's exit code; nothing may be written after it.
func (c *Child) Exit(code int) error {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(int32(code)))
	c.mu.Lock()
	defer c.mu.Unlock()
	return writeFrame(c.w, frameExit, b[:])
}

type streamWriter struct {
	c   *Child
	typ byte
}

func (s streamWriter) Write(p []byte) (int, error) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	written := 0
	for len(p) > 0 {
		n := min(len(p), maxFrame)
		if err := writeFrame(s.c.w, s.typ, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}
