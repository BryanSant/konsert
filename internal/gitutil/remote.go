package gitutil

import (
	"net/url"
	"strings"
)

// NormalizeRemoteURL canonicalizes a git remote URL so equivalent SSH, HTTPS,
// git://, trailing-slash, .git-suffixed, and mixed-case-host URLs compare equal.
//
// Examples:
//   - "git@GitHub.com:Org/Repo.git"          -> "github.com/Org/Repo"
//   - "https://github.com/Org/Repo.git/"     -> "github.com/Org/Repo"
//   - "ssh://git@github.com:22/Org/Repo.git" -> "github.com/Org/Repo"
//   - "ssh://git@github.com:2222/Org/Repo"   -> "github.com:2222/Org/Repo"
//   - "file:///tmp/origin.git"               -> "/tmp/origin"
func NormalizeRemoteURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// Local filesystem path (Unix absolute or Windows drive path) without a scheme.
	if !strings.Contains(s, "://") && (strings.HasPrefix(s, "/") || isWindowsDrivePath(s)) {
		return normalizeLocalPath(s)
	}

	// URL with an explicit scheme: https://, http://, ssh://, git://, file://, ...
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return s
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme == "file" {
			p := u.Path
			if u.Host != "" {
				p = "/" + u.Host + p
			}
			return normalizeLocalPath(p)
		}
		host := strings.ToLower(u.Hostname())
		if host == "" {
			return normalizeLocalPath(u.Path)
		}
		port := u.Port()
		if port != "" && !isDefaultPort(scheme, port) {
			host = host + ":" + port
		}
		path := normalizeRemotePath(u.Path)
		if path == "" {
			return host
		}
		return host + "/" + path
	}

	// SCP-like SSH syntax: [user@]host:path (colon appears before any slash).
	if colon := strings.IndexByte(s, ':'); colon > 0 {
		slash := strings.IndexByte(s, '/')
		if slash < 0 || colon < slash {
			hostPart := s[:colon]
			pathPart := s[colon+1:]
			if at := strings.LastIndexByte(hostPart, '@'); at >= 0 {
				hostPart = hostPart[at+1:]
			}
			host := strings.ToLower(strings.TrimSpace(hostPart))
			path := normalizeRemotePath(pathPart)
			if host == "" {
				return path
			}
			if path == "" {
				return host
			}
			return host + "/" + path
		}
	}

	// Fallback: bare host/path without scheme.
	return normalizeRemotePath(s)
}

func isWindowsDrivePath(s string) bool {
	if len(s) < 3 {
		return false
	}
	c := s[0]
	isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	return isAlpha && s[1] == ':' && (s[2] == '\\' || s[2] == '/')
}

func isDefaultPort(scheme, port string) bool {
	switch scheme {
	case "https":
		return port == "443"
	case "http":
		return port == "80"
	case "ssh":
		return port == "22"
	case "git":
		return port == "9418"
	default:
		return false
	}
}

func normalizeRemotePath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	p = strings.Trim(p, "/")
	if strings.HasSuffix(strings.ToLower(p), ".git") && len(p) > 4 {
		p = p[:len(p)-4]
		p = strings.TrimRight(p, "/")
	}
	return p
}

func normalizeLocalPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	p = strings.TrimRight(p, "/")
	if strings.HasSuffix(strings.ToLower(p), ".git") && len(p) > 4 {
		p = p[:len(p)-4]
		p = strings.TrimRight(p, "/")
	}
	if p == "" {
		return "/"
	}
	return p
}
