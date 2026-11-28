package realname

import "testing"

func TestParseGECOS(t *testing.T) {
	for in, want := range map[string]string{
		"bsant:x:1000:1000:Bryan Sant,,,:/home/bsant:/bin/zsh":               "Bryan Sant",
		"bsant:x:1000:1000:Bryan Sant,Room 1,555-1234,:/home/bsant:/bin/zsh": "Bryan Sant",
		"svc:x:999:999::/var/lib/svc:/usr/sbin/nologin":                      "",
		"Bryan Sant,,,": "Bryan Sant",
		"  ":            "",
	} {
		if got := ParseGECOS(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestPickOrderAndFilters(t *testing.T) {
	for _, c := range []struct {
		name       string
		login      string
		candidates []string
		want       string
	}{
		{"git name wins", "bsant", []string{"Bryan Sant", "B. Sant"}, "Bryan Sant"},
		{"email-only git name is skipped for the OS name", "bsant", []string{"bryan@example.com", "Bryan Sant"}, "Bryan Sant"},
		{"login-equal values are skipped", "bsant", []string{"bsant", "BSANT", "Bryan"}, "Bryan"},
		{"blank values are skipped", "bsant", []string{"", "  ", "Bryan  Sant"}, "Bryan Sant"},
		{"nothing better than the login", "bsant", []string{"", "bsant"}, ""},
		{"a name with an email in it is kept", "bsant", []string{"Bryan Sant <bryan@example.com>"}, "Bryan Sant <bryan@example.com>"},
	} {
		if got := pick(c.login, c.candidates...); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
