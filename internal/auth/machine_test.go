package auth

import (
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestMachineCredentialRoundTrip(t *testing.T) {
	sec, err := NewMachineSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(sec.Secret) != 43 || len(sec.Hash) != 64 || sec.Hash != MachineSecretHash(sec.Secret) {
		t.Fatalf("secret %q hash %q", sec.Secret, sec.Hash)
	}
	id := uuid.Must(uuid.NewV7()).String()
	tok := FormatMachineCredential(id, sec.Secret)
	gotID, gotSecret, ok := ParseMachineCredential(tok)
	if !ok || gotID != id || gotSecret != sec.Secret {
		t.Fatalf("parse %q = %q %q %v", tok, gotID, gotSecret, ok)
	}
	if !VerifyMachineSecret(gotSecret, sec.Hash) {
		t.Fatal("secret does not verify against its hash")
	}
	if VerifyMachineSecret(gotSecret+"x", sec.Hash) || VerifyMachineSecret(gotSecret, "") {
		t.Fatal("wrong secret or empty hash verified")
	}
	// A secret containing '_' (base64url) still splits after the id.
	underscored := strings.Repeat("_a", 22)
	if _, s, ok := ParseMachineCredential(FormatMachineCredential(id, underscored)); !ok || s != underscored {
		t.Fatalf("underscore secret: %q %v", s, ok)
	}
	for _, bad := range []string{"", "smc_", "fleet-token", "smc_" + id, "smc_" + id + "_short",
		"smc_" + strings.ToUpper(id) + "_" + sec.Secret, "smd_" + id + "_" + sec.Secret} {
		if _, _, ok := ParseMachineCredential(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
}

func TestJoinCodes(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`)
	seen := map[string]bool{}
	for range 200 {
		c, err := NewJoinCode()
		if err != nil {
			t.Fatal(err)
		}
		if !shape.MatchString(c) {
			t.Fatalf("join code %q has the wrong shape", c)
		}
		seen[c] = true
		n, ok := NormalizeJoinCode(c)
		if !ok || n != strings.ReplaceAll(c, "-", "") {
			t.Fatalf("normalize(%q) = %q %v", c, n, ok)
		}
	}
	if len(seen) < 195 {
		t.Fatalf("only %d distinct codes out of 200", len(seen))
	}
	cases := map[string]string{
		"abcd-efgh":   "ABCD-EFGH",
		" ab cd efgh": "ABCDEFGH",
		"oI1L-0000":   "0111-0000",
	}
	for in, want := range cases {
		got, ok := NormalizeJoinCode(in)
		if !ok || got != strings.ReplaceAll(want, "-", "") {
			t.Errorf("normalize(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "ABCD-EFG", "ABCD-EFGHJ", "ABCD-EFGU", "ABCD_EFGH"} {
		if _, ok := NormalizeJoinCode(bad); ok {
			t.Errorf("normalized %q", bad)
		}
	}
	if JoinCodeHash("ABCDEFGH") == JoinCodeHash("ABCDEFGJ") || len(JoinCodeHash("ABCDEFGH")) != 64 {
		t.Fatal("join code hash")
	}
}
