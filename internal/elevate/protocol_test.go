package elevate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
)

// The parent and child ends talk over net.Pipe, standing in for the named pipe.
func TestRelayRoundTrip(t *testing.T) {
	parent, child := net.Pipe()
	defer func() { _ = parent.Close() }()
	req := Request{Args: []string{"install", "--service", `--user=.\bob`}, Dir: `C:\work`, Password: "s3cret pass"}

	var got Request
	var wg sync.WaitGroup
	wg.Go(func() {
		defer func() { _ = child.Close() }()
		r, c, err := Accept(child)
		if err != nil {
			t.Error(err)
			return
		}
		got = r
		fmt.Fprint(c.Stdout(), "Created service konsertd\n")
		fmt.Fprint(c.Stderr(), "Warning: something\n")
		big := strings.Repeat("x", maxFrame+10) // split into two frames
		_, _ = io.WriteString(c.Stdout(), big)
		if err := c.Exit(3); err != nil {
			t.Error(err)
		}
	})
	var out, errOut bytes.Buffer
	code, err := Relay(parent, req, &out, &errOut)
	wg.Wait()
	if err != nil || code != 3 {
		t.Fatalf("Relay = %d, %v", code, err)
	}
	if got.Password != req.Password || got.Dir != req.Dir || strings.Join(got.Args, " ") != strings.Join(req.Args, " ") {
		t.Errorf("child got %+v", got)
	}
	if !strings.HasPrefix(out.String(), "Created service konsertd\n") || out.Len() != len("Created service konsertd\n")+maxFrame+10 {
		t.Errorf("stdout: %d bytes, prefix %q", out.Len(), out.String()[:30])
	}
	if errOut.String() != "Warning: something\n" {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestRelayChildDiesWithoutExit(t *testing.T) {
	parent, child := net.Pipe()
	go func() {
		_, c, err := Accept(child)
		if err == nil {
			fmt.Fprint(c.Stdout(), "partial")
		}
		_ = child.Close()
	}()
	var out bytes.Buffer
	_, err := Relay(parent, Request{Args: []string{"install"}}, &out, io.Discard)
	if !errors.Is(err, ErrNoExitCode) {
		t.Fatalf("err = %v, want ErrNoExitCode", err)
	}
	if out.String() != "partial" {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestAcceptRejects(t *testing.T) {
	for name, raw := range map[string][]byte{
		"output frame first": frame(frameStdout, []byte("hi")),
		"no command":         frame(frameRequest, []byte(`{"args":[],"dir":""}`)),
		"bad json":           frame(frameRequest, []byte(`{`)),
		"oversized":          {frameRequest, 0xff, 0xff, 0xff, 0xff},
		"truncated":          {frameRequest, 0, 0, 0, 9, '{'},
	} {
		if _, _, err := Accept(bytes.NewBuffer(raw)); err == nil {
			t.Errorf("%s: Accept succeeded", name)
		}
	}
}

func TestRequestStringRedactsPassword(t *testing.T) {
	s := Request{Args: []string{"install"}, Password: "hunter2"}.String()
	if strings.Contains(s, "hunter2") || !strings.Contains(s, "<redacted>") {
		t.Errorf("String() = %s", s)
	}
	s = fmt.Sprintf("%v", Request{Args: []string{"install"}, Password: "hunter2"})
	if strings.Contains(s, "hunter2") {
		t.Errorf("%%v = %s", s)
	}
}

func frame(typ byte, payload []byte) []byte {
	var b bytes.Buffer
	_ = writeFrame(&b, typ, payload)
	return b.Bytes()
}
