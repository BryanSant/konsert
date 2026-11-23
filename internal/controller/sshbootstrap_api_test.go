package controller

import (
	"net/http"
	"strings"
	"testing"
)

func TestWorkerControllerURL(t *testing.T) {
	cases := []struct {
		public, requested, want string
		wantErr                 bool
	}{
		{public: "https://konsert.example.ts.net", requested: "ws://evil.example/ws/worker", want: "wss://konsert.example.ts.net/ws/worker"},
		{public: "http://10.0.0.2:8420/", want: "ws://10.0.0.2:8420/ws/worker"},
		{public: "https://example.com/konsert", want: "wss://example.com/konsert/ws/worker"},
		{requested: "wss://hub.local/ws/worker", want: "wss://hub.local/ws/worker"},
		{requested: "http://hub.local/ws/worker", wantErr: true},
		{requested: "", wantErr: true},
	}
	for _, tc := range cases {
		got, err := workerControllerURL(tc.public, tc.requested)
		if tc.wantErr {
			if err == nil {
				t.Errorf("workerControllerURL(%q,%q) = %q, want error", tc.public, tc.requested, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("workerControllerURL(%q,%q) = %q, %v; want %q", tc.public, tc.requested, got, err, tc.want)
		}
	}
}

func TestSSHBootstrapRejectsOptionInjection(t *testing.T) {
	_, ts := newSecurityTestServer(t, nil)
	for _, target := range []string{"-oProxyCommand=sh -c 'touch /tmp/pwned'", "host -oProxyCommand=id", "a\nb"} {
		body := `{"target":` + jsonString(target) + `,"controller_url":"ws://127.0.0.1:8420/ws/worker"}`
		resp := doReq(t, http.MethodPost, ts.URL+"/api/ssh/bootstrap", strings.NewReader(body),
			map[string]string{"Content-Type": "application/json"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("target %q: got %d, want 400", target, resp.StatusCode)
		}
	}
	// Without --public-url, a missing controller_url is refused rather than derived
	// from the (spoofable) Host header.
	resp := doReq(t, http.MethodPost, ts.URL+"/api/ssh/bootstrap", strings.NewReader(`{"target":"box"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing controller_url: got %d, want 400", resp.StatusCode)
	}
}

func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}
