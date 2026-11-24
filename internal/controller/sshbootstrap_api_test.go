package controller

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/sshutil"
	"github.com/BryanSant/konsert/internal/store"
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
	srv, ts := newSecurityTestServer(t, nil)
	var boots []sshutil.BootstrapOptions
	srv.launch.bootstrap = func(_ context.Context, o sshutil.BootstrapOptions) sshutil.BootstrapResult {
		boots = append(boots, o)
		return sshutil.BootstrapResult{OK: true, Target: o.Target, WorkerID: "01890a5d-ac96-774b-bcce-b302099a8057", Name: "box"}
	}
	for _, target := range []string{"-oProxyCommand=sh -c 'touch /tmp/pwned'", "host -oProxyCommand=id", "a\nb"} {
		body := `{"target":` + jsonString(target) + `,"controller_url":"ws://127.0.0.1:8420/ws/worker"}`
		resp := doReq(t, http.MethodPost, ts.URL+"/api/ssh/bootstrap", strings.NewReader(body),
			map[string]string{"Content-Type": "application/json"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("target %q: got %d, want 400", target, resp.StatusCode)
		}
	}
	if len(boots) != 0 {
		t.Fatalf("an injected target reached bootstrap: %+v", boots)
	}
	// The SSH transport needs no hub URL: without --public-url or controller_url the
	// machine is still added (with the hub key), and nothing is derived from the
	// (spoofable) Host header.
	resp := doReq(t, http.MethodPost, ts.URL+"/api/ssh/bootstrap", strings.NewReader(`{"target":"box"}`),
		map[string]string{"Content-Type": "application/json", "Host": "evil.example"})
	if resp.StatusCode != http.StatusOK || len(boots) != 1 || boots[0].HubKey == nil {
		t.Fatalf("missing controller_url: got %d, boots %+v", resp.StatusCode, boots)
	}
	srv.attach.stop("01890a5d-ac96-774b-bcce-b302099a8057")
	if w, err := srv.store.GetWorker(context.Background(), "01890a5d-ac96-774b-bcce-b302099a8057"); err != nil || w.ControllerURL != "" || w.Launch != store.LaunchSSH {
		t.Fatalf("recorded worker = %+v, %v", w, err)
	}
}

func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// noSSH keeps tests from ever running a real ssh.
func noSSH(context.Context, string, string, string) (net.Conn, error) {
	return nil, errors.New("no ssh in tests")
}
