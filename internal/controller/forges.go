package controller

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/forge"
	"github.com/BryanSant/konsert/internal/forge/forgejo"
	"github.com/BryanSant/konsert/internal/forge/github"
	"github.com/BryanSant/konsert/internal/forge/gitlab"
	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/secrets"
	"github.com/BryanSant/konsert/internal/store"
)

type pendingDeviceFlow struct {
	Host       string
	Kind       forge.Kind
	DeviceCode string
	Interval   int
	ExpiresAt  time.Time
}

type forgeManager struct {
	resolver   *forge.DefaultResolver
	tokenStore forge.TokenStore
	device     *forge.DeviceFlowClient

	mu      sync.Mutex
	pending map[string]pendingDeviceFlow
}

func newForgeManager(cfg config.ForgesConfig, st store.Store, box *secrets.Box) *forgeManager {
	overrides := make(map[string]forge.Kind, len(cfg.Hosts))
	for h, k := range cfg.Hosts {
		host := forge.NormalizeHost(h)
		if host == "" {
			continue
		}
		if kind, ok := parseForgeKind(k); ok {
			overrides[host] = kind
		}
	}

	tokStore := forge.NewEncryptedSettingsStore(st, box)
	detector := &forge.Detector{Overrides: overrides}
	creds := &forge.CredDiscovery{Store: tokStore}
	resolver := forge.NewResolver(forge.ResolverOptions{
		Detector: detector,
		Creds:    creds,
		Factories: map[forge.Kind]forge.ClientFactory{
			forge.GitHub:  github.Factory,
			forge.GitLab:  gitlab.Factory,
			forge.Forgejo: forgejo.Factory,
		},
	})
	device := &forge.DeviceFlowClient{
		GitHubClientID: cfg.GitHubClientID,
		GitLabClientID: cfg.GitLabClientID,
	}

	return &forgeManager{
		resolver:   resolver,
		tokenStore: tokStore,
		device:     device,
		pending:    make(map[string]pendingDeviceFlow),
	}
}

func parseForgeKind(raw string) (forge.Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(forge.GitHub):
		return forge.GitHub, true
	case string(forge.GitLab):
		return forge.GitLab, true
	case string(forge.Forgejo), "gitea", "codeberg":
		return forge.Forgejo, true
	default:
		return "", false
	}
}

func validForgeHost(host string) (string, bool) {
	h := forge.NormalizeHost(host)
	if h == "" || strings.ContainsAny(h, "/\\ \t\r\n") {
		return "", false
	}
	return h, true
}

// Forges returns the hub's forge.Resolver for delivery modes and other callers.
func (s *Server) Forges() forge.Resolver { return s.forges.resolver }

func (s *Server) registerForgeRoutes() {
	s.mux.Handle("GET /api/forges", s.requireBrowserAuth(s.handleListForges))
	s.mux.Handle("POST /api/forges/detect", s.requireBrowserAuth(s.handleDetectForge))
	s.mux.Handle("PUT /api/forges/{host}/token", s.requireBrowserAuth(s.handlePutForgeToken))
	s.mux.Handle("DELETE /api/forges/{host}/token", s.requireBrowserAuth(s.handleDeleteForgeToken))
	s.mux.Handle("POST /api/forges/{host}/device", s.requireBrowserAuth(s.handleStartForgeDevice))
	s.mux.Handle("GET /api/forges/{host}/device/{id}", s.requireBrowserAuth(s.handlePollForgeDevice))
}

type forgeHostResponse struct {
	Host                string     `json:"host"`
	Kind                forge.Kind `json:"kind"`
	CredentialSource    string     `json:"credential_source"`
	DeviceFlowAvailable bool       `json:"device_flow_available"`
}

// handleListForges serves GET /api/forges. Never returns tokens.
func (s *Server) handleListForges(w http.ResponseWriter, r *http.Request) {
	list := s.forges.resolver.ListHosts(r.Context())
	resp := make([]forgeHostResponse, 0, len(list))
	for _, item := range list {
		resp = append(resp, forgeHostResponse{
			Host:                item.Host,
			Kind:                item.Kind,
			CredentialSource:    item.CredentialSource,
			DeviceFlowAvailable: s.forges.device.Available(item.Kind),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"forges": resp})
}

// handleDetectForge serves POST /api/forges/detect {"remote_url": "..."} or {"host": "..."}.
// Uses the resolver's Detect to identify and record the host without touching credentials.
func (s *Server) handleDetectForge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RemoteURL string `json:"remote_url"`
		Host      string `json:"host"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	raw := strings.TrimSpace(body.RemoteURL)
	if raw == "" {
		raw = strings.TrimSpace(body.Host)
	}
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "remote_url or host is required"})
		return
	}

	target, ok := normalizeDetectTarget(raw)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid remote URL or hostname"})
		return
	}

	repo, err := s.forges.resolver.Detect(r.Context(), target)
	if err != nil {
		if errors.Is(err, forge.ErrUnknownForge) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "could not detect a supported forge (GitHub, GitLab, or Forgejo)"})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"host":     repo.Host,
		"kind":     repo.Kind,
		"api_base": repo.APIBase,
	})
}

// normalizeDetectTarget accepts a full git remote URL, an http(s) origin URL, or a bare
// hostname (with optional port) and returns a remote URL string suitable for forge.Detect.
func normalizeDetectTarget(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if _, _, _, err := forge.ParseRemoteURL(raw); err == nil {
		return raw, true
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "", false
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme != "http" && scheme != "https" {
			return "", false
		}
		return scheme + "://" + u.Host + "/_/repo", true
	}
	if host, ok := validForgeHost(raw); ok {
		return "https://" + host + "/_/repo", true
	}
	return "", false
}

// handlePutForgeToken serves PUT /api/forges/{host}/token {"token": "...", "kind": "..."}.
func (s *Server) handlePutForgeToken(w http.ResponseWriter, r *http.Request) {
	host, ok := validForgeHost(r.PathValue("host"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid forge host"})
		return
	}
	var body struct {
		Token string `json:"token"`
		Kind  string `json:"kind"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	token := strings.TrimSpace(body.Token)
	if token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token is required"})
		return
	}
	var kind forge.Kind
	if strings.TrimSpace(body.Kind) != "" {
		k, valid := parseForgeKind(body.Kind)
		if !valid {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported forge kind (want github, gitlab, or forgejo)"})
			return
		}
		kind = k
	} else if k, known := s.forges.resolver.KindForHost(r.Context(), host); known {
		kind = k
	}

	if err := s.forges.tokenStore.PutToken(r.Context(), forge.StoredToken{
		Host:   host,
		Token:  token,
		Source: "pasted",
		Kind:   kind,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store token"})
		return
	}
	if kind != "" {
		s.forges.resolver.RecordHost(host, kind)
	}
	s.forges.resolver.InvalidateHost(host)

	s.hub.recordAudit(r.Context(), store.AuditEvent{
		Action:   AuditForgeTokenSet,
		Message:  "Stored forge token for " + host,
		MetaJSON: auditMeta(map[string]any{"host": host, "kind": string(kind), "source": "pasted"}),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"host":              host,
		"kind":              kind,
		"credential_source": "pasted",
	})
}

// handleDeleteForgeToken serves DELETE /api/forges/{host}/token.
func (s *Server) handleDeleteForgeToken(w http.ResponseWriter, r *http.Request) {
	host, ok := validForgeHost(r.PathValue("host"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid forge host"})
		return
	}
	if err := s.forges.tokenStore.DeleteToken(r.Context(), host); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete token"})
		return
	}
	s.forges.resolver.InvalidateHost(host)

	s.hub.recordAudit(r.Context(), store.AuditEvent{
		Action:   AuditForgeTokenDelete,
		Message:  "Removed stored forge token for " + host,
		MetaJSON: auditMeta(map[string]any{"host": host}),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"host": host,
	})
}

// handleStartForgeDevice serves POST /api/forges/{host}/device.
func (s *Server) handleStartForgeDevice(w http.ResponseWriter, r *http.Request) {
	host, ok := validForgeHost(r.PathValue("host"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid forge host"})
		return
	}
	var body struct {
		Kind string `json:"kind"`
	}
	if !decodeJSONBody(w, r, &body, true) {
		return
	}
	var kind forge.Kind
	if strings.TrimSpace(body.Kind) != "" {
		k, valid := parseForgeKind(body.Kind)
		if !valid {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported forge kind"})
			return
		}
		kind = k
	} else if k, known := s.forges.resolver.KindForHost(r.Context(), host); known {
		kind = k
	} else {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "forge kind is required for unknown host"})
		return
	}

	res, err := s.forges.device.Start(r.Context(), host, kind)
	if err != nil {
		if errors.Is(err, forge.ErrDeviceFlowUnavailable) {
			writeJSON(w, http.StatusNotImplemented, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	flowID := ids.UUID()
	s.forges.mu.Lock()
	s.forges.pruneExpiredLocked(time.Now().UTC())
	s.forges.pending[flowID] = pendingDeviceFlow{
		Host:       host,
		Kind:       kind,
		DeviceCode: res.DeviceCode,
		Interval:   res.Interval,
		ExpiresAt:  res.ExpiresAt,
	}
	s.forges.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"id":                        flowID,
		"host":                      host,
		"kind":                      kind,
		"user_code":                 res.UserCode,
		"verification_uri":          res.VerificationURI,
		"verification_uri_complete": res.VerificationURIComplete,
		"interval":                  res.Interval,
		"expires_at":                res.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// handlePollForgeDevice serves GET /api/forges/{host}/device/{id}.
func (s *Server) handlePollForgeDevice(w http.ResponseWriter, r *http.Request) {
	host, ok := validForgeHost(r.PathValue("host"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid forge host"})
		return
	}
	flowID := strings.TrimSpace(r.PathValue("id"))

	s.forges.mu.Lock()
	flow, found := s.forges.pending[flowID]
	s.forges.mu.Unlock()
	if !found || flow.Host != host {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device sign-in session not found"})
		return
	}

	res, err := s.forges.device.Poll(r.Context(), flow.Host, flow.Kind, flow.DeviceCode, flow.Interval, flow.ExpiresAt)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	switch res.Status {
	case forge.DeviceAuthorized:
		if err := s.forges.tokenStore.PutToken(context.WithoutCancel(r.Context()), forge.StoredToken{
			Host:   flow.Host,
			Token:  res.AccessToken,
			Source: "device",
			Kind:   flow.Kind,
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store token"})
			return
		}
		s.forges.mu.Lock()
		delete(s.forges.pending, flowID)
		s.forges.mu.Unlock()

		s.forges.resolver.RecordHost(flow.Host, flow.Kind)
		s.forges.resolver.InvalidateHost(flow.Host)
		s.hub.recordAudit(r.Context(), store.AuditEvent{
			Action:   AuditForgeDeviceAuth,
			Message:  "Connected forge " + flow.Host + " via device sign-in",
			MetaJSON: auditMeta(map[string]any{"host": flow.Host, "kind": string(flow.Kind), "source": "device"}),
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"status":            res.Status,
			"host":              flow.Host,
			"kind":              flow.Kind,
			"credential_source": "device",
		})
	case forge.DeviceExpired, forge.DeviceDenied:
		s.forges.mu.Lock()
		delete(s.forges.pending, flowID)
		s.forges.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"status": res.Status,
			"error":  res.Error,
		})
	default:
		if res.Interval > flow.Interval {
			s.forges.mu.Lock()
			if cur, exists := s.forges.pending[flowID]; exists {
				cur.Interval = res.Interval
				s.forges.pending[flowID] = cur
			}
			s.forges.mu.Unlock()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":   res.Status,
			"interval": res.Interval,
		})
	}
}

func (m *forgeManager) pruneExpiredLocked(now time.Time) {
	for id, f := range m.pending {
		if !f.ExpiresAt.IsZero() && now.After(f.ExpiresAt) {
			delete(m.pending, id)
		}
	}
}
