package hubkey

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	pub  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl konsert-hub:hub1"
	pub2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBKqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl konsert-hub:hub1"
	idA  = "01890a5d-ac96-774b-bcce-b302099a8057"
	idB  = "01890a5d-ac96-774b-bcce-b302099a8058"
)

func entry(id string) Entry {
	return Entry{HubID: "hub1", PublicKey: pub, Exe: "/home/me/.local/bin/konsertd", GOOS: "linux", WorkerID: id}
}

func TestLineIsRestrictedToAttach(t *testing.T) {
	got := Line(entry(idA), []string{idA})
	want := `command="/home/me/.local/bin/konsertd attach --allow=` + idA + `",no-pty,no-agent-forwarding,no-port-forwarding,no-X11-forwarding ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl konsert-hub:hub1`
	if got != want {
		t.Fatalf("line =\n%s\nwant\n%s", got, want)
	}
	win := Entry{HubID: "hub1", PublicKey: pub, Exe: `C:\Users\Jo Doe\AppData\Local\Programs\konsertd\konsertd.exe`, GOOS: "windows", WorkerID: idA}
	if l := Line(win, []string{idA}); !strings.HasPrefix(l, `command="\"C:\Users\Jo Doe\AppData\Local\Programs\konsertd\konsertd.exe\" attach --allow=`+idA+`",no-pty`) {
		t.Fatalf("windows line with a space = %s", l)
	}
	posix := entry(idA)
	posix.Exe = "/opt/my apps/konsertd"
	if l := Line(posix, []string{idA}); !strings.HasPrefix(l, `command="'/opt/my apps/konsertd' attach`) {
		t.Fatalf("posix line with a space = %s", l)
	}
}

func TestAddIsIdempotentAndKeepsOtherLines(t *testing.T) {
	orig := "ssh-ed25519 AAAAuser me@laptop\r\n# comment\r\n"
	once, err := Add(orig, entry(idA))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(once, orig) || strings.Count(once, Marker("hub1")) != 1 || !strings.HasSuffix(once, "konsert-hub:hub1\r\n") {
		t.Fatalf("after add (CRLF kept):\n%q", once)
	}
	twice, _ := Add(once, entry(idA))
	if twice != once {
		t.Fatalf("adding again changed the file:\n%q\n%q", once, twice)
	}
	// A second machine on the same account shares the line.
	both, _ := Add(once, entry(idB))
	if strings.Count(both, Marker("hub1")) != 1 || !strings.Contains(both, "--allow="+idA+","+idB+`"`) {
		t.Fatalf("second machine:\n%s", both)
	}
	if ids := Allowed(both, "hub1"); len(ids) != 2 {
		t.Fatalf("Allowed = %v", ids)
	}
	// A regenerated hub key replaces the line in place, keeping the allowed ids.
	e := entry(idA)
	e.PublicKey = pub2
	rekeyed, _ := Add(both, e)
	if strings.Contains(rekeyed, "AAAAIOMqq") || !strings.Contains(rekeyed, "AAAAIBKqq") || !strings.Contains(rekeyed, idB) {
		t.Fatalf("rekeyed:\n%s", rekeyed)
	}
	// Another hub's line is not ours.
	other := Entry{HubID: "hub2", PublicKey: pub, Exe: "/k", GOOS: "linux", WorkerID: idA}
	two, _ := Add(both, other)
	if strings.Count(two, "konsert-hub:") != 2 {
		t.Fatalf("two hubs:\n%s", two)
	}
}

// Mixed line endings and an unterminated last line survive an add and a remove byte for
// byte (a real administrators_authorized_keys had LF lines and one CRLF).
func TestAddRemoveKeepOtherLinesByteForByte(t *testing.T) {
	for _, orig := range []string{
		"ssh-ed25519 AAAAa a@x\nssh-rsa AAAAb b@y\nssh-ed25519 AAAAc c@z\r\n",
		"ssh-ed25519 AAAAa a@x\r\nssh-rsa AAAAb b@y",
		"",
		"\n\n# only comments\n",
	} {
		added, err := Add(orig, entry(idA))
		if err != nil {
			t.Fatal(err)
		}
		removed, changed := Remove(added, "hub1", idA)
		want := orig
		if strings.HasSuffix(orig, "b@y") {
			want = orig + "\r\n" // the unterminated last line got the file's ending
		}
		if !changed || removed != want {
			t.Errorf("round trip of %q:\nadded   %q\nremoved %q", orig, added, removed)
		}
	}
}

func TestAddRejectsBadInput(t *testing.T) {
	bad := []Entry{
		{HubID: "hub 1", PublicKey: pub, Exe: "/k", WorkerID: idA},
		{HubID: "hub1", PublicKey: pub, Exe: "/k", WorkerID: "not-a-uuid"},
		{HubID: "hub1", PublicKey: "ssh-rsa AAAA", Exe: "/k", WorkerID: idA},
		{HubID: "hub1", PublicKey: "ssh-ed25519 !!!", Exe: "/k", WorkerID: idA},
		{HubID: "hub1", PublicKey: pub, Exe: "/k\"; rm -rf /", WorkerID: idA},
		{HubID: "hub1", PublicKey: pub, Exe: "/k\nssh-ed25519 AAAA", WorkerID: idA},
	}
	for _, e := range bad {
		if _, err := Add("", e); err == nil {
			t.Errorf("Add accepted %+v", e)
		}
	}
}

func TestRemoveDropsOnlyOurGrant(t *testing.T) {
	base := "ssh-ed25519 AAAAuser me@laptop\n"
	one, _ := Add(base, entry(idA))
	both, _ := Add(one, entry(idB))

	afterA, changed := Remove(both, "hub1", idA)
	if !changed || strings.Contains(afterA, idA) || !strings.Contains(afterA, "--allow="+idB) || !strings.HasPrefix(afterA, base) {
		t.Fatalf("remove A:\n%s", afterA)
	}
	afterB, changed := Remove(afterA, "hub1", idB)
	if !changed || afterB != base {
		t.Fatalf("remove last id must drop the line:\n%q", afterB)
	}
	if again, changed := Remove(afterB, "hub1", idB); changed || again != afterB {
		t.Fatal("removing twice must be a no-op")
	}
	if _, changed := Remove(both, "hub2", idA); changed {
		t.Fatal("another hub's id must not match")
	}
	if _, changed := Remove(both, "hub1", "01890a5d-ac96-774b-bcce-b302099a8059"); changed {
		t.Fatal("an id that is not allowed changes nothing")
	}
	if all, changed := Remove(both, "hub1", ""); !changed || all != base {
		t.Fatalf("empty worker id drops the whole line: %q", all)
	}
	// A user line that merely mentions the marker mid-line is not ours.
	tricky := "ssh-ed25519 AAAAuser konsert-hub:hub1 is my note\n"
	if _, changed := Remove(tricky, "hub1", ""); changed {
		t.Fatal("only lines whose comment is the marker are managed")
	}
}

func TestAuthorizedKeysPath(t *testing.T) {
	cases := []struct {
		goos        string
		admin       bool
		home, pdata string
		want        string
	}{
		{"linux", false, "/home/me", "", "/home/me/.ssh/authorized_keys"},
		{"darwin", true, "/Users/me/", "", "/Users/me/.ssh/authorized_keys"},
		{"windows", false, `C:\Users\me`, `C:\ProgramData`, `C:\Users\me\.ssh\authorized_keys`},
		{"windows", true, `C:\Users\me`, `C:\ProgramData`, `C:\ProgramData\ssh\administrators_authorized_keys`},
		{"windows", true, `C:\Users\me`, `D:\PD\`, `D:\PD\ssh\administrators_authorized_keys`},
		{"windows", true, `C:\Users\me`, "", `C:\ProgramData\ssh\administrators_authorized_keys`},
	}
	for _, c := range cases {
		if got := AuthorizedKeysPath(c.goos, c.admin, c.home, c.pdata); got != c.want {
			t.Errorf("%s admin=%v: %s, want %s", c.goos, c.admin, got, c.want)
		}
	}
	if AdminKeysSDDL != "D:P(A;;FA;;;SY)(A;;FA;;;BA)" {
		t.Fatal("administrators_authorized_keys must be SYSTEM and Administrators only, not inherited")
	}
}

func TestGenerateWritesOpenSSHKeys(t *testing.T) {
	priv, pubLine, err := Generate("konsert-hub:x")
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(pubLine)
	if len(f) != 3 || f[0] != "ssh-ed25519" || f[2] != "konsert-hub:x" {
		t.Fatalf("public key %q", pubLine)
	}
	blob, _ := base64.StdEncoding.DecodeString(f[1])
	block, _ := pem.Decode(priv)
	if block == nil || block.Type != "OPENSSH PRIVATE KEY" || !bytes.HasPrefix(block.Bytes, []byte("openssh-key-v1\x00")) {
		t.Fatalf("private key PEM: %q", priv)
	}
	// The private key embeds the same public blob, and its private section is padded.
	b := block.Bytes[len("openssh-key-v1\x00"):]
	for range 3 { // cipher, kdf, kdf options
		b = b[4+binary.BigEndian.Uint32(b):]
	}
	if binary.BigEndian.Uint32(b) != 1 {
		t.Fatal("want one key")
	}
	b = b[4:]
	n := binary.BigEndian.Uint32(b)
	if !bytes.Equal(b[4:4+n], blob) {
		t.Fatal("embedded public key differs")
	}
	b = b[4+n:]
	if sec := binary.BigEndian.Uint32(b); sec%8 != 0 || len(b[4:]) != int(sec) {
		t.Fatalf("private section length %d", sec)
	}
}

func TestLoadOrCreateKeepsTheKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hubkey")
	p1, err := LoadOrCreate(path, "konsert-hub:x")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := LoadOrCreate(path, "konsert-hub:x")
	if err != nil || p1 != p2 {
		t.Fatalf("second load: %q vs %q (%v)", p1, p2, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("private key mode %o", fi.Mode().Perm())
		}
	}
	// OpenSSH reads the key and derives the same public key from it.
	if kg, err := exec.LookPath("ssh-keygen"); err == nil {
		out, err := exec.Command(kg, "-y", "-f", path).Output()
		if err != nil {
			t.Fatalf("ssh-keygen -y: %v", err)
		}
		if got := strings.Fields(string(out)); len(got) < 2 || got[1] != strings.Fields(p1)[1] {
			t.Fatalf("ssh-keygen derived %q, want %q", out, p1)
		}
	}
}

func TestInstallAndUninstallFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses $HOME")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Install(entry(idA))
	if err != nil || path != filepath.Join(home, ".ssh", "authorized_keys") {
		t.Fatalf("install: %s %v", path, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o", fi.Mode().Perm())
	}
	if _, err := Install(entry(idA)); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Count(string(b), "\n") != 1 {
		t.Fatalf("file: %q", b)
	}
	if _, changed, err := Uninstall("hub1", idA); err != nil || !changed {
		t.Fatalf("uninstall: %v %v", changed, err)
	}
	if b, _ := os.ReadFile(path); len(b) != 0 {
		t.Fatalf("after uninstall: %q", b)
	}
	if _, changed, err := Uninstall("hub1", idA); err != nil || changed {
		t.Fatalf("second uninstall: %v %v", changed, err)
	}
}
