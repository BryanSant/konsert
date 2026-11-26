package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type stubClient struct {
	kind  Kind
	token string
}

func (s *stubClient) Kind() Kind { return s.kind }
func (s *stubClient) RepoInfo(context.Context, Repo) (RepoInfo, error) {
	return RepoInfo{}, nil
}
func (s *stubClient) OpenPR(context.Context, Repo, OpenPRRequest) (PR, error) { return PR{}, nil }
func (s *stubClient) FindPR(context.Context, Repo, string, string) (PR, error) {
	return PR{}, nil
}
func (s *stubClient) GetPR(context.Context, Repo, int) (PR, error) { return PR{}, nil }
func (s *stubClient) MergePR(context.Context, Repo, int, MergeRequest) (MergeResult, error) {
	return MergeResult{}, nil
}
func (s *stubClient) ReviewComments(context.Context, Repo, int, time.Time) ([]ReviewComment, error) {
	return nil, nil
}
func (s *stubClient) CompareURL(Repo, string, string) string { return "" }

func TestDefaultResolverCachesDetectionAndClients(t *testing.T) {
	ctx := context.Background()
	var probeHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeHits.Add(1)
		if r.URL.Path == "/api/v1/version" {
			_, _ = w.Write([]byte(`{"version":"9.0.0"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	rawSettings := &memSettings{}
	tokStore := NewEncryptedSettingsStore(rawSettings, newTestBox(t))
	_ = tokStore.PutToken(ctx, StoredToken{Host: host, Token: "tok-1", Source: "pasted", Kind: Forgejo})

	var factoryCalls atomic.Int32
	res := NewResolver(ResolverOptions{
		Detector: &Detector{HTTPClient: srv.Client(), Scheme: "http"},
		Creds:    &CredDiscovery{Runner: &fakeCmdRunner{}, Store: tokStore},
		Factories: map[Kind]ClientFactory{
			Forgejo: func(_ Repo, token string, _ *http.Client) Client {
				factoryCalls.Add(1)
				return &stubClient{kind: Forgejo, token: token}
			},
		},
	})

	// First Detect probes; second Detect hits host cache.
	r1, err := res.Detect(ctx, "http://"+host+"/alice/one.git")
	if err != nil || r1.Kind != Forgejo {
		t.Fatalf("first Detect: %+v err=%v", r1, err)
	}
	hitsAfterFirst := probeHits.Load()
	r2, err := res.Detect(ctx, "http://"+host+"/bob/two.git")
	if err != nil || r2.Kind != Forgejo || r2.Owner != "bob" {
		t.Fatalf("second Detect: %+v err=%v", r2, err)
	}
	if probeHits.Load() != hitsAfterFirst {
		t.Fatalf("expected second Detect to use host cache, probes went %d -> %d", hitsAfterFirst, probeHits.Load())
	}

	// First Client builds; second reuses cached client.
	c1, err := res.Client(ctx, r1)
	if err != nil {
		t.Fatalf("Client 1: %v", err)
	}
	c2, err := res.Client(ctx, r2)
	if err != nil {
		t.Fatalf("Client 2: %v", err)
	}
	if c1 != c2 || factoryCalls.Load() != 1 {
		t.Fatalf("expected cached client reuse, factoryCalls=%d", factoryCalls.Load())
	}

	// Updating token invalidates client cache automatically via token hash.
	_ = tokStore.PutToken(ctx, StoredToken{Host: host, Token: "tok-2", Source: "pasted", Kind: Forgejo})
	c3, err := res.Client(ctx, r1)
	if err != nil {
		t.Fatalf("Client 3: %v", err)
	}
	if c3.(*stubClient).token != "tok-2" || factoryCalls.Load() != 2 {
		t.Fatalf("expected new client after token rotation, factoryCalls=%d", factoryCalls.Load())
	}

	hosts := res.ListHosts(ctx)
	if len(hosts) != 1 || hosts[0].Host != host || hosts[0].CredentialSource != "pasted" {
		t.Fatalf("ListHosts: %+v", hosts)
	}
}

func (s *stubClient) UpdatePRBase(context.Context, Repo, int, string) error { return nil }
