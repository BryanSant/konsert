//go:build windows

package elevate

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// childEnv makes the test binary act as the elevated child: ShellExecuteEx passes the
// parent's environment through, and the child gets only the pipe name on its command
// line, as syred's would.
const childEnv = "SYRE_ELEVATE_TEST_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) != "" {
		os.Exit(testChild())
	}
	os.Exit(m.Run())
}

func testChild() int {
	if len(os.Args) != 3 || os.Args[1] != ChildCommand {
		return 90
	}
	conn, err := OpenChildPipe(os.Args[2])
	if err != nil {
		return 91
	}
	defer func() { _ = conn.Close() }()
	req, c, err := Accept(conn)
	if err != nil {
		return 92
	}
	wd, _ := os.Getwd()
	fmt.Fprintf(c.Stdout(), "args=%s dir=%s\n", strings.Join(req.Args, ","), wd)
	fmt.Fprintf(c.Stderr(), "password-len=%d\n", len(req.Password))
	_ = c.Exit(7)
	return 0
}

// launch runs the full parent path (pipe, ShellExecuteEx, client check, relay, exit
// code) with the test binary as the child. verb "" opens it unelevated; "runas" from an
// already elevated process (an administrator over SSH) also skips the prompt.
func launch(t *testing.T, verb string) {
	t.Helper()
	t.Setenv(childEnv, "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	code, err := launcher{verb: verb, file: exe}.run(Request{Args: []string{"install", "--service"}, Dir: dir, Password: "pw-123"}, &out, &errOut)
	if err != nil {
		t.Fatalf("run: %v (stdout %q, stderr %q)", err, out.String(), errOut.String())
	}
	if code != 7 {
		t.Errorf("exit code %d, want 7", code)
	}
	if !strings.Contains(out.String(), "args=install,--service") || !strings.Contains(strings.ToLower(out.String()), strings.ToLower(dir)) {
		t.Errorf("stdout = %q", out.String())
	}
	if errOut.String() != "password-len=6\n" {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestLaunchRelay(t *testing.T) { launch(t, "") }

func TestLaunchRunAsWhenElevated(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("needs an elevated token (runas would show a UAC prompt)")
	}
	launch(t, "runas")
}

func TestPipeIsSingleInstanceAndUserOnly(t *testing.T) {
	name, err := newPipeName()
	if err != nil {
		t.Fatal(err)
	}
	if !pipeNameRE.MatchString(name) {
		t.Fatalf("pipe name %s does not match", name)
	}
	h, err := createPipe(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	// A second server for the same name (a squatter) is refused.
	if h2, err := createPipe(name); err == nil {
		_ = windows.CloseHandle(h2)
		t.Error("a second instance of the pipe was created")
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	tu, _ := windows.GetCurrentProcessToken().GetTokenUser()
	sddl := sd.String()
	if strings.Count(sddl, "(A;") != 1 || !strings.Contains(sddl, ";;;"+tu.User.Sid.String()+")") {
		t.Errorf("pipe DACL %s: want exactly one ACE, for %s", sddl, tu.User.Sid)
	}
}

func TestOpenChildPipeRejectsOtherNames(t *testing.T) {
	for _, n := range []string{`\\.\pipe\other`, `\\server\pipe\syred-elevate-00000000000000000000000000000000`, `C:\x`, pipePrefix + "ZZ"} {
		if _, err := OpenChildPipe(n); err == nil || !strings.Contains(err.Error(), "not a syred elevation pipe") {
			t.Errorf("%s: %v", n, err)
		}
	}
}

func TestDetectOverSSH(t *testing.T) {
	// Informational on a dev box; over SSH (how this suite runs on the test machine)
	// the session is never interactive.
	p := probe()
	t.Logf("probe: %+v, sudo: %s, elevated: %v", p, sudoMode(), windows.GetCurrentProcessToken().IsElevated())
	if p.SSH && Interactive(p) {
		t.Error("an SSH session was considered interactive")
	}
}
