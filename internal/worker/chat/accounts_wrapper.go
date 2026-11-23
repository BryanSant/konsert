package chat

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Wrapper importer: recognizes shell scripts named <agent><N> on PATH (for example
// ~/.local/bin/claude2) that exec the agent CLI with a per-account config directory and,
// optionally, a token file, and registers the equivalent account ("claude2", "Claude 2").
//
// Parsing is deliberately conservative: only plain `NAME=value` assignments whose value
// resolves to an absolute path from literals, $HOME, ~ and `${VAR:-default}` defaults are
// understood. Anything computed ($(...), backticks, unknown variables) is ignored, and a
// script without a recognizable config-directory assignment is not imported.

const maxWrapperSize = 64 << 10

// wrapperDirs returns the directories searched for wrapper scripts.
func (r *AccountRegistry) wrapperDirs(home string) []string {
	if len(r.opts.WrapperDirs) > 0 {
		return r.opts.WrapperDirs
	}
	dirs := filepath.SplitList(os.Getenv("PATH"))
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"))
	}
	return dirs
}

func (r *AccountRegistry) importWrappers(home string) []AccountEntry {
	if runtime.GOOS == "windows" {
		return nil
	}
	seenDir := map[string]bool{}
	seenID := map[string]bool{}
	var out []AccountEntry
	for _, dir := range r.wrapperDirs(home) {
		if dir == "" {
			continue
		}
		clean := filepath.Clean(dir)
		if seenDir[clean] {
			continue
		}
		seenDir[clean] = true
		entries, err := os.ReadDir(clean)
		if err != nil {
			continue
		}
		for _, de := range entries {
			info, n, ok := wrapperName(de.Name())
			if !ok {
				continue
			}
			id := strings.TrimSuffix(info.Binary, "-agent") + strconv.Itoa(n)
			if seenID[id] {
				continue
			}
			path := filepath.Join(clean, de.Name())
			env, tokenFile, reason := parseWrapper(path, info, home)
			if reason != "" {
				r.logOnce("wrapper-skip:"+path+reason, slog.LevelDebug, "not importing agent wrapper script", "path", path, "reason", reason)
				continue
			}
			seenID[id] = true
			e := AccountEntry{
				Account: Account{ID: id, Agent: info.Agent, Label: info.Label + " " + strconv.Itoa(n), Env: env},
				Source:  "wrapper",
			}
			if bin, ok := r.opts.LookPath(info.Binary); ok {
				e.Binary = bin
			} else {
				e.Problem = info.Binary + " is not installed on this worker"
			}
			if tokenFile != "" {
				e.tokenFile, e.tokenEnv = tokenFile, info.TokenEnv
				if err := checkTokenFile(tokenFile); err != nil {
					e.Problem = err.Error()
					r.logOnce("token-file:"+id+err.Error(), slog.LevelError, "agent account token file refused", "id", id, "err", err)
				}
			}
			keys := make([]string, 0, len(env))
			for k := range env {
				keys = append(keys, k)
			}
			r.logOnce("wrapper-import:"+id+":"+path, slog.LevelInfo, "imported agent account from wrapper script",
				"id", id, "label", e.Label, "agent", info.Agent, "path", path, "env_keys", strings.Join(sortedStrings(keys), ","), "token_file", tokenFile != "")
			out = append(out, e)
		}
	}
	return out
}

func sortedStrings(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// wrapperName matches "<binary><N>" for a known agent (N = 2..99).
func wrapperName(name string) (agentInfo, int, bool) {
	for _, a := range knownAgents {
		rest, ok := strings.CutPrefix(name, a.Binary)
		if !ok || rest == "" || len(rest) > 2 {
			continue
		}
		n, err := strconv.Atoi(rest)
		if err != nil || n < 2 || rest[0] == '0' {
			continue
		}
		return a, n, true
	}
	return agentInfo{}, 0, false
}

var (
	assignRe = regexp.MustCompile(`^\s*(?:export\s+|readonly\s+|local\s+|declare\s+(?:-[a-zA-Z]+\s+)?)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	// inlineAssignRe finds NAME=value inside a line (env/exec arguments, array items).
	inlineAssignRe = regexp.MustCompile(`(?:^|[\s(])([A-Za-z_][A-Za-z0-9_]*)=("(?:[^"\\]|\\.)*"|'[^']*'|[^\s)'"]+)`)
	shebangRe      = regexp.MustCompile(`^#!\s*\S*(?:/|\s)(?:env\s+)?(?:ba|z|da|k)?sh\b`)
	execRe         = regexp.MustCompile(`(?m)^\s*(?:exec\b|.*\bexec\s)`)
)

// parseWrapper returns the account env and token file a wrapper script sets up, or a
// reason why it is not imported.
func parseWrapper(path string, info agentInfo, home string) (map[string]string, string, string) {
	if len(info.ConfigEnv) == 0 {
		return nil, "", "agent has no per-account config directory"
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, "", "not a regular file"
	}
	if st.Size() > maxWrapperSize {
		return nil, "", "too large for a wrapper script"
	}
	if st.Mode().Perm()&0o111 == 0 {
		return nil, "", "not executable"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", "unreadable"
	}
	text := string(raw)
	firstLine, _, _ := strings.Cut(text, "\n")
	if !shebangRe.MatchString(firstLine) {
		return nil, "", "not a shell script"
	}
	if !execRe.MatchString(text) || !mentionsBinary(text, info.Binary) {
		return nil, "", "does not exec " + info.Binary
	}

	vars := map[string]string{"HOME": home}
	known := map[string]bool{"HOME": home != ""}
	inline := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if m := assignRe.FindStringSubmatch(line); m != nil {
			name := m[1]
			val, ok := shellWord(m[2])
			if ok {
				if res, ok := resolveShell(val, vars, known, home); ok {
					vars[name], known[name] = res, true
					continue
				}
			}
			delete(vars, name)
			known[name] = false
			continue
		}
		for _, m := range inlineAssignRe.FindAllStringSubmatch(line, -1) {
			inline[m[1]] = m[2]
		}
	}
	// Inline assignments (e.g. `env CLAUDE_CONFIG_DIR="$DIR" claude`) are resolved
	// against the final variable table.
	resolved := func(name string) (string, bool) {
		if raw, ok := inline[name]; ok {
			if w, ok := shellWord(raw); ok {
				if v, ok := resolveShell(w, vars, known, home); ok {
					return v, true
				}
			}
			return "", false
		}
		if known[name] {
			return vars[name], true
		}
		return "", false
	}

	env := map[string]string{}
	for _, k := range info.ConfigEnv {
		if v, ok := resolved(k); ok && filepath.IsAbs(v) {
			env[k] = v
		}
	}
	if _, ok := env[info.ConfigEnv[0]]; !ok {
		return nil, "", "no " + info.ConfigEnv[0] + " assignment with a literal path"
	}
	tokenFile := ""
	if info.TokenEnv != "" {
		chosen := ""
		for name, ok := range known {
			if ok && strings.HasSuffix(name, "TOKEN_FILE") && filepath.IsAbs(vars[name]) && (chosen == "" || name < chosen) {
				chosen, tokenFile = name, vars[name]
			}
		}
	}
	return env, tokenFile, ""
}

// mentionsBinary reports whether the script refers to the agent binary as a command or a
// path component (".../claude", " claude ", "command -v claude").
func mentionsBinary(text, binary string) bool {
	re := regexp.MustCompile(`(?:^|[\s/"'=(])` + regexp.QuoteMeta(binary) + `(?:$|[\s"';)}])`)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// shellWord extracts the first shell word of s: a double-quoted string (kept with its
// $-expansions), a single-quoted literal, or a bare word. Words with command
// substitution are refused.
func shellWord(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", true
	}
	var w string
	switch s[0] {
	case '"':
		end := -1
		for i := 1; i < len(s); i++ {
			if s[i] == '\\' {
				i++
				continue
			}
			if s[i] == '"' {
				end = i
				break
			}
		}
		if end < 0 {
			return "", false
		}
		w = s[1:end]
		if rest := strings.TrimSpace(s[end+1:]); rest != "" && !strings.HasPrefix(rest, "#") && !strings.HasPrefix(rest, ";") {
			return "", false
		}
	case '\'':
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", false
		}
		lit := s[1 : end+1]
		// Single-quoted text is literal; mark it so resolveShell does not expand it.
		if strings.ContainsAny(lit, "$`") {
			return "", false
		}
		return lit, true
	default:
		w = s
		if i := strings.IndexAny(w, " \t;"); i >= 0 {
			rest := strings.TrimSpace(w[i:])
			w = w[:i]
			if rest != "" && !strings.HasPrefix(rest, "#") && !strings.HasPrefix(rest, ";") {
				return "", false
			}
		}
	}
	if strings.Contains(w, "$(") || strings.Contains(w, "`") {
		return "", false
	}
	return w, true
}

// resolveShell expands $VAR, ${VAR}, ${VAR:-default} / ${VAR-default} (always the default:
// the wrapper's own override variables are not consulted) and a leading "~".
func resolveShell(w string, vars map[string]string, known map[string]bool, home string) (string, bool) {
	if strings.HasPrefix(w, "~") {
		if home == "" || (len(w) > 1 && w[1] != '/') {
			return "", false
		}
		w = home + w[1:]
	}
	var b strings.Builder
	for i := 0; i < len(w); i++ {
		c := w[i]
		if c == '\\' && i+1 < len(w) {
			i++
			b.WriteByte(w[i])
			continue
		}
		if c != '$' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(w) {
			return "", false
		}
		if w[i+1] == '{' {
			depth, end := 0, -1
			for j := i + 1; j < len(w); j++ {
				if w[j] == '{' {
					depth++
				} else if w[j] == '}' {
					depth--
					if depth == 0 {
						end = j
						break
					}
				}
			}
			if end < 0 {
				return "", false
			}
			inner := w[i+2 : end]
			name, def, hasDef := inner, "", false
			if k := strings.Index(inner, ":-"); k >= 0 {
				name, def, hasDef = inner[:k], inner[k+2:], true
			} else if k := strings.IndexByte(inner, '-'); k >= 0 {
				name, def, hasDef = inner[:k], inner[k+1:], true
			}
			if !validVarName(name) {
				return "", false
			}
			if hasDef && name != "HOME" {
				v, ok := resolveShell(def, vars, known, home)
				if !ok {
					return "", false
				}
				b.WriteString(v)
			} else {
				if !known[name] {
					return "", false
				}
				b.WriteString(vars[name])
			}
			i = end
			continue
		}
		j := i + 1
		for j < len(w) && (w[j] == '_' || (w[j] >= 'A' && w[j] <= 'Z') || (w[j] >= 'a' && w[j] <= 'z') || (j > i+1 && w[j] >= '0' && w[j] <= '9')) {
			j++
		}
		name := w[i+1 : j]
		if !validVarName(name) || !known[name] {
			return "", false
		}
		b.WriteString(vars[name])
		i = j - 1
	}
	return b.String(), true
}

func validVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}
