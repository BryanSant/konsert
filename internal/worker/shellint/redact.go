package shellint

import (
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Redacted replaces a secret value in a redacted command.
const Redacted = "…"

// maxRedactedLen caps the hub-bound command summary (in runes).
const maxRedactedLen = 1000

// Redact returns a summary of a command line that is safe to send to the hub: the
// program, subcommand and ordinary arguments are kept; values that look like secrets are
// replaced with "…":
//
//   - values of secret-named flags: --password=x, --token x, -Password x, --api-key x
//   - secret-named assignments: FOO_TOKEN=x, export AWS_SECRET_ACCESS_KEY=x,
//     $env:GITHUB_TOKEN = 'x', password=x
//   - program-specific flags: mysql -pSECRET, docker/podman login -p x, sshpass -p x,
//     curl -u user:x, secret HTTP headers (-H "Authorization: …"),
//     ConvertTo-SecureString 'x'
//   - credentials in URLs (scheme://user:x@host, ?token=x) and token-looking user parts
//   - opaque values: known token formats (ghp_, github_pat_, sk-, xox?-, glpat-, AKIA…,
//     JWTs …), 32+ hex digits, and long high-entropy base64-like strings
//
// Text the user pipes in (echo secret | docker login --password-stdin) cannot be told
// apart from ordinary arguments and is kept.
func Redact(cmd string) string {
	toks := tokenize(cmd)
	repl := make(map[int]string) // token index -> replacement text
	var (
		program    string
		sub        string
		wrapper    string // sudo/env/... seen at command start
		skipNext   bool   // wrapper option argument (sudo -u root)
		await      bool   // next word is the value of a secret flag
		awaitHdr   bool   // next word is an HTTP header (curl -H)
		awaitUser  bool   // next word is user:password (curl -u)
		positional int
	)
	reset := func() {
		program, sub, wrapper, skipNext, await, awaitHdr, awaitUser, positional = "", "", "", false, false, false, false, 0
	}
	for i, t := range toks {
		if t.op {
			reset()
			continue
		}
		w := t.val
		if program == "" {
			if skipNext {
				skipNext = false
				continue
			}
			if name, val, ok := assignment(w); ok {
				if r, changed := redactAssignment(name, val, "="); changed {
					repl[i] = r
				}
				continue
			}
			if wrapper != "" && strings.HasPrefix(w, "-") {
				if wrapper == "sudo" && (w == "-u" || w == "-g" || w == "-C" || w == "-D" || w == "-h" || w == "-p" || w == "-U") {
					skipNext = true
				}
				continue
			}
			base := progName(w)
			switch base {
			case "sudo", "doas", "env", "time", "nohup", "nice", "exec", "command", "builtin", "xargs", "caffeinate", "stdbuf", "ionice":
				wrapper = base
				continue
			}
			program = base
			if strings.HasPrefix(w, "$") && i+1 < len(toks) && !toks[i+1].op && toks[i+1].val == "=" {
				// PowerShell assignment: $env:GITHUB_TOKEN = 'x'
				if secretName(psVarName(w)) {
					await = true
				}
			}
			if r, changed := redactValue(w, program); changed {
				repl[i] = r
			}
			continue
		}

		if isRedirect(w) {
			// "< file", "2>&1": not an argument (and not a flag's value).
			await, awaitHdr, awaitUser = false, false, false
			continue
		}
		switch {
		case await:
			await = false
			if w == "=" {
				await = true // "$x = value": the value follows
				continue
			}
			if !looksLikeFlag(w) {
				repl[i] = Redacted
				continue
			}
		case awaitHdr:
			awaitHdr = false
			if r, changed := redactHeader(w); changed {
				repl[i] = r
			}
			continue
		case awaitUser:
			awaitUser = false
			if u, _, ok := strings.Cut(w, ":"); ok {
				repl[i] = u + ":" + Redacted
			}
			continue
		}

		if looksLikeFlag(w) {
			r, changed, next := redactFlag(w, program, sub)
			if changed {
				repl[i] = r
			}
			switch next {
			case nextSecret:
				await = true
			case nextHeader:
				awaitHdr = true
			case nextUser:
				awaitUser = true
			}
			continue
		}

		positional++
		if positional == 1 && isWord(w) {
			sub = strings.ToLower(w)
		}
		if program == "convertto-securestring" && positional == 1 {
			repl[i] = Redacted
			continue
		}
		if name, val, ok := strings.Cut(w, "="); ok && name != "" && !strings.Contains(name, "://") {
			if r, changed := redactAssignment(name, val, "="); changed {
				repl[i] = r
				continue
			}
		}
		if r, changed := redactValue(w, program); changed {
			repl[i] = r
		}
	}
	if len(repl) == 0 {
		return truncateRunes(cmd, maxRedactedLen)
	}
	var b strings.Builder
	last := 0
	for i, t := range toks {
		r, ok := repl[i]
		if !ok {
			continue
		}
		b.WriteString(cmd[last:t.start])
		b.WriteString(quoteIfNeeded(r))
		last = t.end
	}
	b.WriteString(cmd[last:])
	return truncateRunes(b.String(), maxRedactedLen)
}

type nextKind int

const (
	nextNone nextKind = iota
	nextSecret
	nextHeader
	nextUser
)

// redactFlag handles one flag word.
func redactFlag(w, program, sub string) (string, bool, nextKind) {
	dashes := 1
	if strings.HasPrefix(w, "--") {
		dashes = 2
	}
	body := w[dashes:]
	if dashes == 1 && isMySQL(program) && len(body) > 1 && body[0] == 'p' {
		return "-p" + Redacted, true, nextNone // -pSECRET (a bare -p prompts)
	}
	name, val, attached := strings.Cut(body, "=")
	sep := "="
	if !attached && dashes == 1 {
		// PowerShell: -Password:value
		if n, v, ok := strings.Cut(body, ":"); ok && len(n) > 1 {
			name, val, attached, sep = n, v, true, ":"
		}
	}
	lname := strings.ToLower(name)

	// Short options whose meaning depends on the program.
	if dashes == 1 && len(name) >= 1 {
		switch {
		case name == "p" && !attached && (program == "sshpass" ||
			(sub == "login" && (program == "docker" || program == "podman" || program == "nerdctl" || program == "helm"))):
			return w, false, nextSecret
		}
	}
	if program == "curl" || program == "http" || program == "https" {
		switch {
		case (dashes == 1 && name == "H") || lname == "header":
			if attached {
				if r, changed := redactHeader(val); changed {
					return w[:dashes] + name + sep + r, true, nextNone
				}
				return w, false, nextNone
			}
			return w, false, nextHeader
		case (dashes == 1 && name == "u") || lname == "user" || lname == "proxy-user":
			if attached {
				if u, _, ok := strings.Cut(val, ":"); ok {
					return w[:dashes] + name + sep + u + ":" + Redacted, true, nextNone
				}
				return w, false, nextNone
			}
			return w, false, nextUser
		}
	}
	if (dashes == 2 || len(name) > 2) && secretName(name) {
		if attached {
			if val == "" {
				return w, false, nextNone
			}
			return w[:dashes] + name + sep + Redacted, true, nextNone
		}
		return w, false, nextSecret
	}
	if attached && val != "" {
		if r, changed := redactValue(val, program); changed {
			return w[:dashes] + name + sep + r, true, nextNone
		}
	}
	return w, false, nextNone
}

// redactAssignment redacts NAME=value when the name or the value looks secret.
func redactAssignment(name, val, sep string) (string, bool) {
	if val == "" {
		return "", false
	}
	if secretName(name) {
		return name + sep + Redacted, true
	}
	if r, changed := redactValue(val, ""); changed {
		return name + sep + r, true
	}
	return "", false
}

// redactHeader redacts the value of a secret HTTP header ("Authorization: Bearer x").
func redactHeader(h string) (string, bool) {
	name, val, ok := strings.Cut(h, ":")
	if !ok || strings.TrimSpace(val) == "" {
		return "", false
	}
	n := strings.ToLower(strings.TrimSpace(name))
	if secretName(n) || n == "authorization" || n == "proxy-authorization" || n == "x-api-key" || n == "cookie" || n == "set-cookie" {
		return name + ": " + Redacted, true
	}
	if r, changed := redactValue(strings.TrimSpace(val), ""); changed {
		return name + ": " + r, true
	}
	return "", false
}

var (
	urlCredRe   = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)([^/@:\s]*)(:[^/@\s]*)?@`)
	queryRe     = regexp.MustCompile(`([?&;])([A-Za-z0-9_.\-]+)=([^&#\s]*)`)
	tokenRe     = regexp.MustCompile(`^(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_\-]{16,}|xox[abposr]-[A-Za-z0-9\-]{10,}|glpat-[A-Za-z0-9_\-]{16,}|(?:AKIA|ASIA)[0-9A-Z]{16}|AIza[0-9A-Za-z_\-]{30,}|npm_[A-Za-z0-9]{30,}|hf_[A-Za-z0-9]{30,}|eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]*)$`)
	hexRe       = regexp.MustCompile(`^(?:0x)?[0-9a-fA-F]{32,}$`)
	base64ishRe = regexp.MustCompile(`^[A-Za-z0-9+_\-]{24,}={0,2}$`)
)

// redactValue redacts secrets inside a single value: URL credentials, secret query
// parameters, or the whole value when it looks like a token.
func redactValue(v, program string) (string, bool) {
	if v == "" {
		return v, false
	}
	if looksLikeToken(v, program) {
		return Redacted, true
	}
	out := v
	if strings.Contains(out, "://") {
		out = urlCredRe.ReplaceAllStringFunc(out, func(m string) string {
			sm := urlCredRe.FindStringSubmatch(m)
			scheme, user, pass := sm[1], sm[2], sm[3]
			if pass != "" {
				return scheme + user + ":" + Redacted + "@"
			}
			if looksLikeToken(user, "") {
				return scheme + Redacted + "@"
			}
			return m
		})
	}
	if strings.ContainsAny(out, "?&") {
		out = queryRe.ReplaceAllStringFunc(out, func(m string) string {
			sm := queryRe.FindStringSubmatch(m)
			if sm[3] != "" && (secretName(sm[2]) || looksLikeToken(sm[3], "")) {
				return sm[1] + sm[2] + "=" + Redacted
			}
			return m
		})
	}
	return out, out != v
}

// looksLikeToken reports whether v as a whole looks like a credential.
func looksLikeToken(v, program string) bool {
	if len(v) < 16 {
		return false
	}
	if tokenRe.MatchString(v) {
		return true
	}
	if hexRe.MatchString(v) {
		// Commit hashes are hex too; they are not secrets.
		return program != "git"
	}
	if !base64ishRe.MatchString(v) {
		return false
	}
	var digit, upper, lower bool
	for _, r := range v {
		switch {
		case unicode.IsDigit(r):
			digit = true
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsLower(r):
			lower = true
		}
	}
	return digit && upper && lower && entropy(v) >= 4.0
}

// entropy is the Shannon entropy of s in bits per byte.
func entropy(s string) float64 {
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	var h float64
	n := float64(len(s))
	for _, c := range counts {
		if c > 0 {
			p := float64(c) / n
			h -= p * math.Log2(p)
		}
	}
	return h
}

// secretName reports whether a flag, variable, header or parameter name denotes a
// secret: FOO_TOKEN, --api-key, -AccessToken, client_secret, _authToken, password …
func secretName(name string) bool {
	parts := nameParts(name)
	if len(parts) == 0 {
		return false
	}
	if parts[0] == "no" {
		return false // --no-password
	}
	switch parts[len(parts)-1] {
	case "file", "files", "path", "stdin", "env", "dir", "cmd", "command", "helper", "prompt",
		"type", "method", "mode", "id", "name", "length", "provider", "source", "url", "endpoint", "policy":
		return false // --password-stdin, --token-file, --key-id …
	}
	for i, p := range parts {
		switch p {
		case "password", "passwd", "passphrase", "pass", "pw", "secret", "secrets", "token", "tokens",
			"apikey", "credential", "credentials", "cookie", "cookies", "authorization", "bearer",
			"privatekey", "accesskey", "secretkey", "otp", "auth", "sig", "signature":
			return true
		case "key":
			if i == len(parts)-1 {
				return true // API_KEY, --secret-key, --key
			}
		}
	}
	return false
}

// nameParts splits a name into lower-case words at "-", "_", ".", ":" and camelCase
// boundaries.
func nameParts(name string) []string {
	var parts []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			parts = append(parts, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(name)
	for i, r := range rs {
		switch {
		case r == '-' || r == '_' || r == '.' || r == ':' || r == '/' || r == '$':
			flush()
		case unicode.IsUpper(r) && i > 0 && unicode.IsLower(rs[i-1]):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return parts
}

// psVarName extracts the variable name of a PowerShell variable ($env:FOO -> FOO).
func psVarName(w string) string {
	w = strings.TrimPrefix(w, "$")
	if i := strings.LastIndexByte(w, ':'); i >= 0 {
		w = w[i+1:]
	}
	return w
}

// assignment reports whether w is a shell variable assignment NAME=value.
func assignment(w string) (name, val string, ok bool) {
	name, val, ok = strings.Cut(w, "=")
	if !ok || name == "" {
		return "", "", false
	}
	for i, r := range name {
		if !(r == '_' || unicode.IsLetter(r) || (i > 0 && unicode.IsDigit(r))) {
			return "", "", false
		}
	}
	return name, val, true
}

// isRedirect reports whether w is a redirection operator, possibly with its target
// (<, >>, 2>, &>, 2>&1, >out.txt).
func isRedirect(w string) bool {
	w = strings.TrimLeft(w, "0123456789&")
	return strings.HasPrefix(w, "<") || strings.HasPrefix(w, ">")
}

func looksLikeFlag(w string) bool {
	if len(w) < 2 || w[0] != '-' || w == "--" {
		return false
	}
	c := w[1]
	return !(c >= '0' && c <= '9') // -1 is a number
}

func isWord(w string) bool {
	if w == "" {
		return false
	}
	for _, r := range w {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == ':' || r == '.') {
			return false
		}
	}
	return true
}

func isMySQL(program string) bool {
	switch program {
	case "mysql", "mariadb", "mysqldump", "mysqladmin", "mysqlimport", "mysqlshow", "mysqlcheck", "mariadb-dump":
		return true
	}
	return false
}

// progName is the lower-case base name of a program word, without .exe.
func progName(w string) string {
	base := filepath.Base(strings.ReplaceAll(w, `\`, "/"))
	return strings.TrimSuffix(strings.ToLower(base), ".exe")
}

func quoteIfNeeded(s string) string {
	if s == "" || !strings.ContainsAny(s, " \t\n'\"|&;<>()$`\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	rs := []rune(s)
	return string(rs[:n-1]) + Redacted
}

// token is a word or operator of a command line. start/end span the raw text.
type token struct {
	start, end int
	val        string // unquoted word
	op         bool   // | || & && ; ;; ( ) or newline
}

// tokenize splits a command line into words and control operators, approximately as
// POSIX shells and PowerShell do: quotes, backslash escapes and $(…) are honoured.
func tokenize(s string) []token {
	var toks []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '\\' && i+1 < len(s) && s[i+1] == '\n':
			i += 2
		case c == '\n' || c == ';' || c == '|' || c == '&' || c == '(' || c == ')':
			j := i + 1
			if j < len(s) && (s[j] == c || (c == '|' && s[j] == '&')) && c != '(' && c != ')' && c != '\n' {
				j++
			}
			toks = append(toks, token{start: i, end: j, val: s[i:j], op: true})
			i = j
		default:
			start := i
			var val strings.Builder
			depth := 0
		word:
			for i < len(s) {
				c := s[i]
				switch {
				case depth == 0 && (c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ';' || c == '|' || c == '(' || c == ')'):
					break word
				case depth == 0 && c == '&' && !(i > start && (s[i-1] == '>' || s[i-1] == '<')):
					break word
				case c == '\'':
					j := strings.IndexByte(s[i+1:], '\'')
					if j < 0 {
						val.WriteString(s[i+1:])
						i = len(s)
						break word
					}
					val.WriteString(s[i+1 : i+1+j])
					i += j + 2
				case c == '"':
					i++
					for i < len(s) && s[i] != '"' {
						if (s[i] == '\\' || s[i] == '`') && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
							i++
						}
						val.WriteByte(s[i])
						i++
					}
					i++ // closing quote
				case c == '\\' && i+1 < len(s):
					val.WriteByte(s[i+1])
					i += 2
				case c == '$' && i+1 < len(s) && s[i+1] == '(':
					depth++
					val.WriteString("$(")
					i += 2
				case depth > 0 && c == '(':
					depth++
					val.WriteByte(c)
					i++
				case depth > 0 && c == ')':
					depth--
					val.WriteByte(c)
					i++
				default:
					val.WriteByte(c)
					i++
				}
			}
			if i > len(s) {
				i = len(s)
			}
			toks = append(toks, token{start: start, end: i, val: val.String()})
		}
	}
	return toks
}
