//go:build windows

package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWriteSecretFileIsUserOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konsert", "konsertd.yaml")
	if err := WriteSecretFile(path, []byte("name: x\n")); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	sddl := sd.String()
	user, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	// Protected DACL with exactly SYSTEM and the user; nothing inherited (no Users,
	// Authenticated Users or Everyone).
	if !strings.HasPrefix(sddl, "D:P") {
		t.Errorf("DACL not protected: %s", sddl)
	}
	for _, want := range []string{";;;SY)", ";;;" + user + ")"} {
		if !strings.Contains(sddl, want) {
			t.Errorf("DACL %s missing %s", sddl, want)
		}
	}
	for _, bad := range []string{";;;BU)", ";;;AU)", ";;;WD)", "ID;"} {
		if strings.Contains(sddl, bad) {
			t.Errorf("DACL %s grants %s", sddl, bad)
		}
	}
	if strings.Count(sddl, "(A;") != 2 {
		t.Errorf("DACL %s should have exactly two ACEs", sddl)
	}
	if data, _ := os.ReadFile(path); string(data) != "name: x\n" {
		t.Errorf("content = %q", data)
	}
}

func TestServiceSDDL(t *testing.T) {
	if got := serviceFileSDDL("S-1-5-21-9"); got != "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;S-1-5-21-9)" {
		t.Errorf("serviceFileSDDL = %s", got)
	}
	for _, s := range []string{serviceFileSDDL("S-1-5-21-9"), serviceDirSDDL("S-1-5-21-9"), userFileSDDL("S-1-5-21-9")} {
		if _, err := windows.SecurityDescriptorFromString(s); err != nil {
			t.Errorf("%s does not parse: %v", s, err)
		}
	}
}

func TestNormalizeAccount(t *testing.T) {
	scm, lookup := normalizeAccount("builder")
	if scm != `.\builder` || !strings.HasSuffix(lookup, `\builder`) || strings.HasPrefix(lookup, ".") {
		t.Errorf("normalizeAccount = %q, %q", scm, lookup)
	}
	if scm, _ := normalizeAccount(`CORP\bob`); scm != `CORP\bob` {
		t.Errorf("domain account changed: %q", scm)
	}
}
