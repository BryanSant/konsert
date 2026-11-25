package forge

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// ClientFactory constructs a Client for a given Repo and token.
type ClientFactory func(r Repo, token string, httpClient *http.Client) Client

// ResolverOptions configures a DefaultResolver.
type ResolverOptions struct {
	Detector   *Detector
	Creds      *CredDiscovery
	Factories  map[Kind]ClientFactory
	HTTPClient *http.Client
}

// HostStatus describes a forge host known to the resolver (without ever exposing tokens).
type HostStatus struct {
	Host             string `json:"host"`
	Kind             Kind   `json:"kind"`
	CredentialSource string `json:"credential_source"` // "git-credential" | "gh" | "glab" | "device" | "pasted" | "none"
}

type cachedHost struct {
	kind    Kind
	apiBase string
}

type cachedClient struct {
	kind      Kind
	apiBase   string
	tokenHash [32]byte
	client    Client
}

// DefaultResolver ties together forge detection, credential discovery, and per-kind
// Client constructors with a per-host cache.
type DefaultResolver struct {
	detector   *Detector
	creds      *CredDiscovery
	factories  map[Kind]ClientFactory
	httpClient *http.Client

	mu      sync.RWMutex
	hosts   map[string]cachedHost
	clients map[string]cachedClient
}

// NewResolver creates a DefaultResolver.
func NewResolver(opts ResolverOptions) *DefaultResolver {
	det := opts.Detector
	if det == nil {
		det = &Detector{}
	}
	creds := opts.Creds
	if creds == nil {
		creds = &CredDiscovery{}
	}
	factories := make(map[Kind]ClientFactory, len(opts.Factories))
	for k, f := range opts.Factories {
		factories[k] = f
	}
	return &DefaultResolver{
		detector:   det,
		creds:      creds,
		factories:  factories,
		httpClient: opts.HTTPClient,
		hosts:      make(map[string]cachedHost),
		clients:    make(map[string]cachedClient),
	}
}

// Detect parses remoteURL into a Repo, caching the host's detected Kind and APIBase.
func (res *DefaultResolver) Detect(ctx context.Context, remoteURL string) (Repo, error) {
	host, owner, name, err := ParseRemoteURL(remoteURL)
	if err != nil {
		return Repo{}, err
	}

	res.mu.RLock()
	cached, ok := res.hosts[host]
	res.mu.RUnlock()
	if ok {
		return Repo{
			Kind:    cached.kind,
			Host:    host,
			Owner:   owner,
			Name:    name,
			APIBase: cached.apiBase,
		}, nil
	}

	repo, err := res.detector.Detect(ctx, remoteURL)
	if err != nil {
		return Repo{}, err
	}

	res.mu.Lock()
	res.hosts[repo.Host] = cachedHost{kind: repo.Kind, apiBase: repo.APIBase}
	res.mu.Unlock()
	return repo, nil
}

// Client returns an authenticated Client for r's host, or ErrNoCredential.
func (res *DefaultResolver) Client(ctx context.Context, r Repo) (Client, error) {
	host := NormalizeHost(r.Host)
	if host == "" {
		return nil, ErrUnknownForge
	}
	r.Host = host
	if r.APIBase == "" {
		r.APIBase = APIBaseFor(res.detector.scheme(), r.Kind, host)
	}

	factory := res.factories[r.Kind]
	if factory == nil {
		return nil, fmt.Errorf("%w: no adapter for %q", ErrUnknownForge, r.Kind)
	}

	cred, err := res.creds.Discover(ctx, host, r.Kind)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(cred.Token))

	res.mu.RLock()
	if c, ok := res.clients[host]; ok && c.kind == r.Kind && c.apiBase == r.APIBase && c.tokenHash == hash {
		res.mu.RUnlock()
		return c.client, nil
	}
	res.mu.RUnlock()

	client := factory(r, cred.Token, res.httpClient)

	res.mu.Lock()
	res.hosts[host] = cachedHost{kind: r.Kind, apiBase: r.APIBase}
	res.clients[host] = cachedClient{
		kind:      r.Kind,
		apiBase:   r.APIBase,
		tokenHash: hash,
		client:    client,
	}
	res.mu.Unlock()
	return client, nil
}

// RecordHost marks a host as seen with the given kind (if non-empty).
func (res *DefaultResolver) RecordHost(host string, kind Kind) {
	host = NormalizeHost(host)
	if host == "" || kind == "" {
		return
	}
	res.mu.Lock()
	res.hosts[host] = cachedHost{
		kind:    kind,
		apiBase: APIBaseFor(res.detector.scheme(), kind, host),
	}
	res.mu.Unlock()
}

// InvalidateHost clears the cached client for host (e.g. after updating or deleting a token).
func (res *DefaultResolver) InvalidateHost(host string) {
	host = NormalizeHost(host)
	res.mu.Lock()
	delete(res.clients, host)
	res.mu.Unlock()
}

// KindForHost returns the best-known forge Kind for host from overrides, seen cache,
// stored tokens, or well-known hosts.
func (res *DefaultResolver) KindForHost(ctx context.Context, host string) (Kind, bool) {
	host = NormalizeHost(host)
	if k, ok := res.detector.overrideFor(host); ok {
		return k, true
	}
	res.mu.RLock()
	if c, ok := res.hosts[host]; ok && c.kind != "" {
		res.mu.RUnlock()
		return c.kind, true
	}
	res.mu.RUnlock()
	if res.creds != nil && res.creds.Store != nil {
		if st, ok, err := res.creds.Store.GetToken(ctx, host); err == nil && ok && st.Kind != "" {
			return st.Kind, true
		}
	}
	return KnownHostKind(host)
}

// ListHosts returns all hosts seen, configured in overrides, or present in the token store,
// along with their kind and active credential source ("none" when unauthenticated).
func (res *DefaultResolver) ListHosts(ctx context.Context) []HostStatus {
	hostKinds := make(map[string]Kind)

	// 1. Configured overrides
	if res.detector != nil {
		for h, k := range res.detector.Overrides {
			h = NormalizeHost(h)
			if h != "" {
				hostKinds[h] = k
			}
		}
	}

	// 2. Stored tokens
	if res.creds != nil && res.creds.Store != nil {
		if stored, err := res.creds.Store.ListTokens(ctx); err == nil {
			for _, st := range stored {
				h := NormalizeHost(st.Host)
				if h == "" {
					continue
				}
				if _, exists := hostKinds[h]; !exists {
					k := st.Kind
					if k == "" {
						k, _ = KnownHostKind(h)
					}
					hostKinds[h] = k
				}
			}
		}
	}

	// 3. Hosts seen via Detect / Client
	res.mu.RLock()
	for h, c := range res.hosts {
		if _, exists := hostKinds[h]; !exists {
			hostKinds[h] = c.kind
		}
	}
	res.mu.RUnlock()

	hosts := make([]string, 0, len(hostKinds))
	for h := range hostKinds {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	out := make([]HostStatus, 0, len(hosts))
	for _, h := range hosts {
		k := hostKinds[h]
		src := "none"
		if cred, err := res.creds.Discover(ctx, h, k); err == nil && cred.Source != "" {
			src = cred.Source
		}
		out = append(out, HostStatus{
			Host:             h,
			Kind:             k,
			CredentialSource: src,
		})
	}
	return out
}
