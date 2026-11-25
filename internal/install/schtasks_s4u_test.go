package install

import (
	"bytes"
	"context"
	"encoding/xml"
	"strings"
	"testing"
)

type taskDoc struct {
	Triggers struct {
		Boot  *struct{} `xml:"BootTrigger"`
		Logon *struct{} `xml:"LogonTrigger"`
		Time  *struct{} `xml:"TimeTrigger"`
	} `xml:"Triggers"`
	Principal struct {
		LogonType string `xml:"LogonType"`
		RunLevel  string `xml:"RunLevel"`
	} `xml:"Principals>Principal"`
	Description string `xml:"RegistrationInfo>Description"`
}

func parseTask(t *testing.T, x string) taskDoc {
	t.Helper()
	var d taskDoc
	if err := xml.Unmarshal([]byte(x[strings.Index(x, "<Task "):]), &d); err != nil {
		t.Fatalf("task XML: %v\n%s", err, x)
	}
	return d
}

// An elevated install registers an S4U task (runs whether or not the user is signed
// in: boot, logon and keep-alive triggers); a non-elevated one a logon task that says
// when konsertd starts and how to make it headless (KON-224K5).
func TestSchtasksS4UWhenElevated(t *testing.T) {
	for _, elevated := range []bool{true, false} {
		in, r := testInstaller(t)
		in.Env.Elevated = elevated
		if err := writeFile(in.Env.SystemRoot+`\System32\conhost.exe`, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		spec := testSpec("s4u")
		_, def, err := schtasksBackend{}.render(in, spec)
		if err != nil {
			t.Fatal(err)
		}
		d := parseTask(t, string(def))
		if err := in.install(context.Background(), schtasksBackend{}, spec); err != nil {
			t.Fatal(err)
		}
		out := in.Out.(*bytes.Buffer).String()
		if elevated {
			if d.Principal.LogonType != TaskLogonS4U || d.Triggers.Boot == nil || d.Triggers.Logon == nil || d.Triggers.Time == nil {
				t.Errorf("elevated task = %+v", d)
			}
			if !strings.Contains(d.Description, "whether or not the user is signed in") || !strings.Contains(out, "S4U") || strings.Contains(out, HeadlessHint) {
				t.Errorf("elevated: description %q, output %q", d.Description, out)
			}
		} else {
			if d.Principal.LogonType != TaskLogonInteractive || d.Triggers.Boot != nil || d.Triggers.Logon == nil {
				t.Errorf("non-elevated task = %+v", d)
			}
			if !strings.Contains(out, "starts when you sign in") || !strings.Contains(out, HeadlessHint) {
				t.Errorf("non-elevated output = %q", out)
			}
		}
		if d.Principal.RunLevel != "LeastPrivilege" {
			t.Errorf("run level = %q", d.Principal.RunLevel)
		}
		if !r.called(`schtasks /Run /TN \konsertd-s4u`) {
			t.Errorf("not started: %v", r.calls)
		}
	}
}

func TestSchtasksStatusSaysWhenItRuns(t *testing.T) {
	for logon, want := range map[string]string{
		TaskLogonS4U:         "whether or not you are signed in",
		TaskLogonInteractive: "only while you are signed in",
	} {
		in, r := testInstaller(t)
		r.out[`schtasks /Query /TN \konsertd-x /FO`] = `"\konsertd-x","N/A","Running"` + "\r\n"
		r.out[`schtasks /Query /TN \konsertd-x /XML`] = RenderTaskXML(TaskXMLParams{URI: `\konsertd-x`, UserSID: "S-1", LogonType: logon})
		st, err := in.status(context.Background(), schtasksBackend{}, "x")
		if err != nil || !st.Installed || !st.Running || !strings.Contains(st.Mechanism, want) {
			t.Errorf("%s: status = %+v, %v", logon, st, err)
		}
	}
	if got := taskLogonType("<Principal><LogonType> S4U </LogonType></Principal>"); got != TaskLogonS4U {
		t.Fatalf("taskLogonType = %q", got)
	}
	if got := taskLogonType("nothing"); got != "" {
		t.Fatalf("taskLogonType = %q", got)
	}
}
