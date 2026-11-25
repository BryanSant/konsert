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
	for _, s := range []string{serviceSecretFileSDDL("S-1-5-21-9"), serviceDirSDDL("S-1-5-21-9"), userFileSDDL("S-1-5-21-9")} {
		if _, err := windows.SecurityDescriptorFromString(s); err != nil {
			t.Errorf("%s does not parse: %v", s, err)
		}
	}
}

// The SDDL tokens of the service account's ACE parse to exactly ServiceAccountRights
// (SERVICE_QUERY_CONFIG|SERVICE_QUERY_STATUS|SERVICE_STOP|DELETE), and the config
// file ACE to FILE_GENERIC_READ|DELETE.
func TestServiceAccountACEMask(t *testing.T) {
	for _, tc := range []struct {
		sddl string
		want uint32
	}{
		{"D:" + serviceAccountACE("S-1-5-21-1-2-3-1001"), ServiceAccountRights},
		{"D:(A;;0x130089;;;S-1-5-21-1-2-3-1001)", windows.FILE_GENERIC_READ | windows.DELETE},
	} {
		sd, err := windows.SecurityDescriptorFromString(tc.sddl)
		if err != nil {
			t.Fatal(err)
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, 0, &ace); err != nil {
			t.Fatal(err)
		}
		if uint32(ace.Mask) != tc.want {
			t.Errorf("%s: mask %#x, want %#x", tc.sddl, ace.Mask, tc.want)
		}
	}
	want := uint32(windows.SERVICE_QUERY_CONFIG | windows.SERVICE_QUERY_STATUS | windows.SERVICE_STOP | windows.DELETE)
	for name, got := range map[string]uint32{"ServiceAccountRights": ServiceAccountRights, "serviceRemovalAccess": serviceRemovalAccess} {
		if got != want {
			t.Errorf("%s = %#x, want %#x", name, got, want)
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
