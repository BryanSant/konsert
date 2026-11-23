package controller

import (
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/worker"
)

const (
	// maxJSONBodyBytes caps ordinary REST/MCP request bodies.
	maxJSONBodyBytes = 1 << 20
	// maxMCPBodyBytes caps Streamable HTTP MCP request bodies (prompts can be long).
	maxMCPBodyBytes = 4 << 20
	// maxAttachmentBodyBytes caps POST /api/attachments: a 12 MiB file either as
	// multipart or base64-in-JSON (4/3 expansion), plus envelope slack.
	maxAttachmentBodyBytes = worker.MaxAttachmentBytes*4/3 + 1<<20

	// csrfHeader may be sent by API clients that cannot set a JSON Content-Type.
	// Browsers cannot attach custom headers cross-origin without a CORS preflight,
	// which this server never approves.
	csrfHeader = "X-Konsert-Request"
)

// buildOriginPatterns turns the configured public URL and extra allowed origins into
// coder/websocket-style origin patterns: a bare host pattern ("localhost:4200",
// "*.example.ts.net") is matched against the Origin host, and a pattern containing
// "://" is matched against "scheme://host".
func buildOriginPatterns(cfg config.ControllerConfig) []string {
	var out []string
	add := func(p string) {
		p = strings.TrimRight(strings.TrimSpace(p), "/")
		if p == "" {
			return
		}
		for _, existing := range out {
			if strings.EqualFold(existing, p) {
				return
			}
		}
		out = append(out, p)
	}
	if u, err := url.Parse(strings.TrimSpace(cfg.PublicURL)); err == nil && u.Host != "" {
		add(u.Host)
	}
	for _, o := range cfg.AllowedOrigins {
		o = strings.TrimSpace(o)
		if strings.Contains(o, "://") {
			if u, err := url.Parse(o); err == nil && u.Host != "" {
				add(u.Scheme + "://" + u.Host)
				continue
			}
		}
		add(o)
	}
	return out
}

// originMatchesPatterns reports whether origin (a full Origin header value) matches
// one of the explicitly configured patterns.
func originMatchesPatterns(u *url.URL, patterns []string) bool {
	for _, p := range patterns {
		target := u.Host
		if strings.Contains(p, "://") {
			target = u.Scheme + "://" + u.Host
		}
		if ok, err := path.Match(strings.ToLower(p), strings.ToLower(target)); err == nil && ok {
			return true
		}
	}
	return false
}

// checkOrigin mirrors coder/websocket's origin authentication for plain HTTP requests.
// It returns allowed=false for a cross-origin browser request, and explicit=true when
// the Origin matched a configured pattern (as opposed to the request's own Host).
// A request without an Origin header (CLI, workers, MCP clients) is allowed.
func checkOrigin(r *http.Request, patterns []string) (allowed, explicit bool) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true, false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		// Includes the opaque "null" origin sent by sandboxed frames and file: pages.
		return false, false
	}
	if originMatchesPatterns(u, patterns) {
		return true, true
	}
	return strings.EqualFold(r.Host, u.Host), false
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// requestHasBody reports whether a request carries (or may carry) a body.
func requestHasBody(r *http.Request) bool {
	return r.ContentLength > 0 || (r.ContentLength < 0 && r.Body != nil && r.Body != http.NoBody)
}

// guard applies request-level protections in front of the mux:
//   - body size limits (413) on /api/* and /mcp;
//   - cross-site request rejection (403) for state-changing /api/* and /mcp requests,
//     using Origin and Sec-Fetch-Site;
//   - a JSON-only Content-Type rule (415) for state-changing /api/* requests with a
//     body (multipart is accepted only for POST /api/attachments), so that "simple"
//     cross-site form/text posts can never reach a handler.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isAPI := strings.HasPrefix(r.URL.Path, "/api/")
		isMCP := r.URL.Path == "/mcp" || strings.HasPrefix(r.URL.Path, "/mcp/")
		if !isAPI && !isMCP {
			next.ServeHTTP(w, r)
			return
		}

		limit := int64(maxJSONBodyBytes)
		switch {
		case isMCP:
			limit = maxMCPBodyBytes
		case r.URL.Path == "/api/attachments":
			limit = maxAttachmentBodyBytes
		}
		if r.ContentLength > limit {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}

		if isSafeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		// Per-chat MCP endpoints authenticate only with the chat's bearer token,
		// never an ambient browser credential, so cross-site checks do not apply
		// (agent CLIs on workers send no Origin, but a proxy or client might).
		if strings.HasPrefix(r.URL.Path, "/mcp/chat/") {
			next.ServeHTTP(w, r)
			return
		}

		allowed, explicit := checkOrigin(r, s.originPatterns)
		if !allowed {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request rejected"})
			return
		}
		if site := strings.ToLower(r.Header.Get("Sec-Fetch-Site")); (site == "cross-site" || site == "same-site") && !explicit {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request rejected"})
			return
		}

		if isAPI && requestHasBody(r) && r.Header.Get(csrfHeader) == "" {
			mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			ok := mt == "application/json" ||
				(mt == "multipart/form-data" && r.Method == http.MethodPost && r.URL.Path == "/api/attachments")
			if !ok {
				writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// decodeJSON decodes the request body into v, writing 413 when the body exceeds the
// limit and 400 for malformed JSON. It reports whether decoding succeeded.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		if isBodyTooLarge(err) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return false
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return false
	}
	return true
}

func isBodyTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// clientIP returns the direct peer IP of the request (no proxy headers are trusted).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
