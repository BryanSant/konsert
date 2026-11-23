package worker

import "testing"

func TestParseIOPlatformUUID(t *testing.T) {
	out := `+-o J413AP  <class IOPlatformExpertDevice, id 0x100000201, registered>
    {
      "IOPlatformSerialNumber" = "XXXXXXXX"
      "IOPlatformUUID" = "05E0132A-5418-53EC-98D9-698AE6AAFD47"
    }`
	if got := parseIOPlatformUUID(out); got != "05E0132A-5418-53EC-98D9-698AE6AAFD47" {
		t.Fatalf("parseIOPlatformUUID = %q", got)
	}
	if got := parseIOPlatformUUID("no uuid here"); got != "" {
		t.Fatalf("parseIOPlatformUUID(garbage) = %q", got)
	}
	if osMachineHash() == "" {
		t.Fatal("no IOPlatformUUID-based fingerprint on this Mac")
	}
}
