package forge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRemoteURL(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantHost  string
		wantOwner string
		wantName  string
		wantErr   bool
	}{
		{
			name:      "https github with .git",
			raw:       "https://github.com/octocat/hello-world.git",
			wantHost:  "github.com",
			wantOwner: "octocat",
			wantName:  "hello-world",
		},
		{
			name:      "https github without .git and trailing slash",
			raw:       "https://GitHub.com/octocat/hello-world/",
			wantHost:  "github.com",
			wantOwner: "octocat",
			wantName:  "hello-world",
		},
		{
			name:      "https with userinfo and default port",
			raw:       "https://user@github.com:443/octocat/hello-world.git",
			wantHost:  "github.com",
			wantOwner: "octocat",
			wantName:  "hello-world",
		},
		{
			name:      "https with custom port",
			raw:       "https://git.internal.example:8443/team/project.git",
			wantHost:  "git.internal.example:8443",
			wantOwner: "team",
			wantName:  "project",
		},
		{
			name:      "scp-style github",
			raw:       "git@github.com:octocat/hello-world.git",
			wantHost:  "github.com",
			wantOwner: "octocat",
			wantName:  "hello-world",
		},
		{
			name:      "scp-style gitlab nested groups",
			raw:       "git@gitlab.com:acme/platform/backend/api.git",
			wantHost:  "gitlab.com",
			wantOwner: "acme/platform/backend",
			wantName:  "api",
		},
		{
			name:      "ssh URL with custom ssh port stripped",
			raw:       "ssh://git@gitlab.example.com:2222/group/sub/repo.git",
			wantHost:  "gitlab.example.com",
			wantOwner: "group/sub",
			wantName:  "repo",
		},
		{
			name:      "git protocol",
			raw:       "git://codeberg.org/alice/demo.git",
			wantHost:  "codeberg.org",
			wantOwner: "alice",
			wantName:  "demo",
		},
		{
			name:    "empty",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "missing repo name",
			raw:     "https://github.com/octocat",
			wantErr: true,
		},
		{
			name:    "unsupported scheme",
			raw:     "file:///var/tmp/repo.git",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, owner, name, err := ParseRemoteURL(tc.raw)
			if tc.wantErr {
				if !errors.Is(err, ErrUnknownForge) {
					t.Fatalf("err = %v, want ErrUnknownForge", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tc.wantHost || owner != tc.wantOwner || name != tc.wantName {
				t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)", host, owner, name, tc.wantHost, tc.wantOwner, tc.wantName)
			}
		})
	}
}

func TestDetectKnownHostsAndOverrides(t *testing.T) {
	ctx := context.Background()
	det := &Detector{
		Overrides: map[string]Kind{
			"git.corp.example":    GitHub,
			"forge.local.example": Forgejo,
		},
	}

	tests := []struct {
		remote  string
		want    Kind
		apiBase string
	}{
		{"https://github.com/a/b.git", GitHub, "https://api.github.com"},
		{"git@gitlab.com:g/sub/p.git", GitLab, "https://gitlab.com/api/v4"},
		{"https://codeberg.org/a/b", Forgejo, "https://codeberg.org/api/v1"},
		{"git@git.corp.example:org/app.git", GitHub, "https://git.corp.example/api/v3"},
		{"https://forge.local.example/u/r.git", Forgejo, "https://forge.local.example/api/v1"},
	}

	for _, tc := range tests {
		repo, err := det.Detect(ctx, tc.remote)
		if err != nil {
			t.Fatalf("Detect(%q): %v", tc.remote, err)
		}
		if repo.Kind != tc.want || repo.APIBase != tc.apiBase {
			t.Fatalf("Detect(%q) = (%s, %s), want (%s, %s)", tc.remote, repo.Kind, repo.APIBase, tc.want, tc.apiBase)
		}
	}
}

func TestDetectProbing(t *testing.T) {
	ctx := context.Background()

	t.Run("gitlab 200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v4/version" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"version":"17.5.0","revision":"abc"}`))
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()

		host := strings.TrimPrefix(srv.URL, "http://")
		det := &Detector{HTTPClient: srv.Client(), Scheme: "http"}
		repo, err := det.Detect(ctx, "http://"+host+"/grp/sub/proj.git")
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if repo.Kind != GitLab || repo.APIBase != "http://"+host+"/api/v4" || repo.Owner != "grp/sub" {
			t.Fatalf("got %+v", repo)
		}
	})

	t.Run("gitlab 401 with header", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v4/version" {
				w.Header().Set("X-Gitlab-Meta", `{"correlation_id":"1"}`)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()

		host := strings.TrimPrefix(srv.URL, "http://")
		det := &Detector{HTTPClient: srv.Client(), Scheme: "http"}
		repo, err := det.Detect(ctx, "http://"+host+"/org/proj.git")
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if repo.Kind != GitLab {
			t.Fatalf("got kind %q, want gitlab", repo.Kind)
		}
	})

	t.Run("forgejo 200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/version" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"version":"9.0.1"}`))
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()

		host := strings.TrimPrefix(srv.URL, "http://")
		det := &Detector{HTTPClient: srv.Client(), Scheme: "http"}
		repo, err := det.Detect(ctx, "http://"+host+"/alice/repo.git")
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if repo.Kind != Forgejo || repo.APIBase != "http://"+host+"/api/v1" {
			t.Fatalf("got %+v", repo)
		}
	})

	t.Run("github enterprise meta", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v3/meta" {
				w.Header().Set("X-GitHub-Enterprise-Version", "3.14.0")
				_, _ = w.Write([]byte(`{"verifiable_password_authentication":false}`))
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()

		host := strings.TrimPrefix(srv.URL, "http://")
		det := &Detector{HTTPClient: srv.Client(), Scheme: "http"}
		repo, err := det.Detect(ctx, "http://"+host+"/org/repo.git")
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if repo.Kind != GitHub || repo.APIBase != "http://"+host+"/api/v3" {
			t.Fatalf("got %+v", repo)
		}
	})

	t.Run("unknown host returns ErrUnknownForge", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		defer srv.Close()

		host := strings.TrimPrefix(srv.URL, "http://")
		det := &Detector{HTTPClient: srv.Client(), Scheme: "http"}
		_, err := det.Detect(ctx, "http://"+host+"/org/repo.git")
		if !errors.Is(err, ErrUnknownForge) {
			t.Fatalf("err = %v, want ErrUnknownForge", err)
		}
	})
}
