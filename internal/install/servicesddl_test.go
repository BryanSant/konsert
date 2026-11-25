package install

import (
	"strings"
	"testing"
)

const testSID = "S-1-5-21-1111-2222-3333-1001"

func TestServiceAccountRights(t *testing.T) {
	// SERVICE_QUERY_CONFIG|SERVICE_QUERY_STATUS|SERVICE_STOP|DELETE.
	if ServiceAccountRights != 0x10025 {
		t.Errorf("ServiceAccountRights = %#x, want 0x10025", ServiceAccountRights)
	}
	if got := serviceAccountACE(testSID); got != "(A;;CCLCWPSD;;;"+testSID+")" {
		t.Errorf("serviceAccountACE = %s", got)
	}
}

func TestServiceDACLWithAccount(t *testing.T) {
	// The default DACL of a service CreateService made, as GetSecurityInfo prints it.
	def := "D:(A;;CCLCSWRPWPDTLOCRRC;;;SY)(A;;CCDCLCSWRPWPDTLOCRSDRCWDWO;;;BA)(A;;CCLCSWLOCRRC;;;IU)(A;;CCLCSWLOCRRC;;;SU)"
	ace := "(A;;CCLCWPSD;;;" + testSID + ")"
	for _, tc := range []struct {
		name, in, want string
	}{
		{"default DACL", def, def + ace},
		{"reinstall replaces the old ACE", def + "(A;;CCLC;;;" + testSID + ")", def + ace},
		{"case-insensitive SID match", "D:(A;;CC;;;" + strings.ToLower(testSID) + ")(A;;FA;;;SY)", "D:(A;;FA;;;SY)" + ace},
		{"keeps other accounts", "D:(A;;CCLC;;;S-1-5-21-9-9-9-1002)", "D:(A;;CCLC;;;S-1-5-21-9-9-9-1002)" + ace},
		{"keeps DACL flags", "D:PAI(A;;FA;;;SY)", "D:PAI(A;;FA;;;SY)" + ace},
		{"keeps the SACL after the DACL", def + "S:(AU;FA;CCDCLCSWRPWPDTLOCRSDRCWDWO;;;WD)", def + ace + "S:(AU;FA;CCDCLCSWRPWPDTLOCRSDRCWDWO;;;WD)"},
		{"keeps owner and group before it", "O:SYG:SY" + def, "O:SYG:SY" + def + ace},
		{"deny ACEs stay ahead of the new allow", "D:(D;;WP;;;BG)(A;;FA;;;SY)", "D:(D;;WP;;;BG)(A;;FA;;;SY)" + ace},
		{"empty DACL", "D:", "D:" + ace},
	} {
		got, err := serviceDACLWithAccount(tc.in, testSID)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, got, tc.want)
		}
	}
	for name, in := range map[string]string{
		"no DACL":           "O:SY",
		"unclosed ACE":      "D:(A;;FA;;;SY",
		"text between ACEs": "D:(A;;FA;;;SY)x(A;;FA;;;BA)",
	} {
		if _, err := serviceDACLWithAccount(in, testSID); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := serviceDACLWithAccount(def, "S-1-5-21-1)(A;;GA;;;WD"); err == nil {
		t.Error("an SID with SDDL syntax was accepted")
	}
}

func TestServiceSecretFileSDDL(t *testing.T) {
	got := serviceSecretFileSDDL(testSID)
	want := "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x130089;;;" + testSID + ")"
	if got != want {
		t.Errorf("serviceSecretFileSDDL = %s", got)
	}
}

func TestInstanceOfConfig(t *testing.T) {
	for name, want := range map[string]string{"konsertd.yaml": "", "konsertd-box.yaml": "box"} {
		if got, ok := instanceOfConfig(name); !ok || got != want {
			t.Errorf("%s: %q, %v", name, got, ok)
		}
	}
	for _, name := range []string{"konsertd.service.json", "other.yaml", "konsertdx.yaml"} {
		if _, ok := instanceOfConfig(name); ok {
			t.Errorf("%s accepted", name)
		}
	}
}
