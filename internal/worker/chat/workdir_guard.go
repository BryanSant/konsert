package chat

import (
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"strings"

	"github.com/BryanSant/syre/internal/protocol"
)

// pickAllowOption selects an allow option from an elicitation's options (preferring
// allow_once over allow_always). Returns "" if no allow option is present.
func pickAllowOption(opts []protocol.AgentElicitationOption) string {
	for _, kind := range []string{"allow_once", "allow_always"} {
		for _, o := range opts {
			if o.Kind == kind {
				return o.ID
			}
		}
	}
	return ""
}

// workdirAllowOption returns an allow option ID when the permission request targets
// safe in-workdir operations (reads, edits, non-destructive shell commands inside the
// ticket workdir), or "" to escalate to the user (needs_you).
func (s *acpSession) workdirAllowOption(tc *protocol.AgentToolCall, raw jsontext.Value, opts []protocol.AgentElicitationOption) string {
	if !s.opts.AutoApproveWorkdir {
		return ""
	}
	if !allowInWorkdir(s.opts.Cwd, s.opts.EngineWorktrees, tc, raw) {
		return ""
	}
	return pickAllowOption(opts)
}

// allowInWorkdir reports whether a permission request for tc (with optional rawInput JSON)
// stays strictly inside workdir (or worktrees) and is not an obviously destructive command.
// It fails closed (returns false) when the workdir is unknown, when any target path escapes
// the workdir, or when a shell command is destructive or references paths outside workdir.
func allowInWorkdir(workdir string, worktrees []protocol.TaskWorktree, tc *protocol.AgentToolCall, rawInput jsontext.Value) bool {
	if tc == nil {
		return false
	}
	roots := canonicalWorkdirRoots(workdir, worktrees)
	if len(roots) == 0 {
		return false
	}

	kind := strings.ToLower(strings.TrimSpace(tc.Kind))
	name := strings.ToLower(strings.TrimSpace(tc.Name))

	// Never auto-approve network/web fetch tools as "in-workdir".
	if kind == "fetch" || name == "webfetch" || name == "websearch" {
		return false
	}

	fields := extractToolInputMap(rawInput, tc.Input)

	// Gather all file/directory paths referenced by the tool call.
	var paths []string
	paths = append(paths, tc.Paths...)
	for _, key := range []string{
		"path", "file_path", "filePath", "target_path", "targetPath",
		"TargetFile", "AbsolutePath", "DirectoryPath", "SearchPath",
		"notebook_path", "cwd", "Cwd", "workdir", "working_directory",
	} {
		if v := mapStringFold(fields, key); v != "" {
			paths = append(paths, v)
		}
	}

	// Determine effective cwd for resolving relative paths/commands.
	effectiveCwd := roots[0]
	if subCwd := firstNonEmptyMapString(fields, "cwd", "Cwd", "workdir", "working_directory"); subCwd != "" {
		resolved, ok := resolvePathInRoots(subCwd, effectiveCwd, roots)
		if !ok {
			return false
		}
		effectiveCwd = resolved
	}

	// Every referenced path must stay inside the allowed roots.
	for _, p := range paths {
		if _, ok := resolvePathInRoots(p, effectiveCwd, roots); !ok {
			return false
		}
	}

	// Check shell commands.
	cmd := firstNonEmptyMapString(fields, "CommandLine", "command", "cmd")
	isExec := kind == "execute" || isShellToolName(name)
	if cmd == "" && isExec {
		cmd = commandFromTitle(tc.Title)
	}
	if cmd != "" {
		return isSafeInWorkdirCommand(cmd, effectiveCwd, roots)
	}
	if isExec {
		// Execute tool with no parseable command: fail closed.
		return false
	}

	// File-mutating tools must have at least one verified in-workdir path.
	if kind == "edit" || kind == "delete" || kind == "move" || isEditToolName(name) {
		return len(paths) > 0
	}

	// Read / search / think tools with no outside paths are safe in-workdir.
	if kind == "read" || kind == "search" || kind == "think" || isReadToolName(name) {
		return true
	}

	// Other tools: allow only if they carried at least one verified in-workdir path.
	return len(paths) > 0
}

func isShellToolName(name string) bool {
	switch name {
	case "bash", "powershell", "shell", "run_command", "execute_command", "terminal", "exec":
		return true
	}
	return strings.Contains(name, "shell") || strings.Contains(name, "terminal") || strings.Contains(name, "run_command")
}

func isEditToolName(name string) bool {
	switch name {
	case "edit", "write", "multiedit", "notebookedit", "write_file", "edit_file", "replace_file_content", "multi_replace_file_content", "write_to_file", "delete_file":
		return true
	}
	return strings.Contains(name, "write") || strings.Contains(name, "edit") || strings.Contains(name, "delete")
}

func isReadToolName(name string) bool {
	switch name {
	case "read", "read_file", "view_file", "glob", "grep", "grep_search", "find_by_name", "list_dir", "list_files":
		return true
	}
	return strings.Contains(name, "read") || strings.Contains(name, "search") || strings.Contains(name, "list") || strings.Contains(name, "view")
}

func commandFromTitle(title string) string {
	t := strings.TrimSpace(title)
	for _, prefix := range []string{"Run: ", "Run ", "Execute: ", "Execute ", "Bash: ", "Shell: "} {
		if strings.HasPrefix(t, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(t, prefix))
		}
	}
	return ""
}

func canonicalWorkdirRoots(workdir string, worktrees []protocol.TaskWorktree) []string {
	var raw []string
	if strings.TrimSpace(workdir) != "" {
		raw = append(raw, workdir)
	}
	for _, wt := range worktrees {
		if strings.TrimSpace(wt.WorktreePath) != "" {
			raw = append(raw, wt.WorktreePath)
		}
	}
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = filepath.Clean(p)
		} else {
			abs = filepath.Clean(abs)
		}
		if !seen[abs] {
			seen[abs] = true
			out = append(out, abs)
		}
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			real = filepath.Clean(real)
			if !seen[real] {
				seen[real] = true
				out = append(out, real)
			}
		}
	}
	for _, r := range raw {
		add(r)
	}
	return out
}

func extractToolInputMap(raw jsontext.Value, fallback string) map[string]any {
	out := map[string]any{}
	parseInto := func(data []byte) {
		if len(data) == 0 {
			return
		}
		var m map[string]any
		if unmarshal(data, &m) != nil || len(m) == 0 {
			return
		}
		// ACP permission payloads nest tool input under "rawInput".
		if nested, ok := m["rawInput"].(map[string]any); ok {
			for k, v := range nested {
				if _, exists := out[k]; !exists {
					out[k] = v
				}
			}
		}
		for k, v := range m {
			if k == "rawInput" {
				continue
			}
			if _, exists := out[k]; !exists {
				out[k] = v
			}
		}
	}
	parseInto(raw)
	if fallback != "" {
		parseInto([]byte(fallback))
	}
	return out
}

func mapStringFold(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			if s, ok := v.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func firstNonEmptyMapString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := mapStringFold(m, k); v != "" {
			return v
		}
	}
	return ""
}

// resolvePathInRoots resolves p against baseDir and reports whether it lies inside at
// least one root (also resolving symlinks on the longest existing prefix).
func resolvePathInRoots(p, baseDir string, roots []string) (string, bool) {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, `"'`)
	if p == "" {
		return baseDir, true
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", false
		}
		if p == "~" {
			p = home
		} else {
			p = filepath.Join(home, p[2:])
		}
	}
	var full string
	if isAbsAnyOS(p) {
		full = filepath.Clean(p)
	} else {
		full = filepath.Clean(filepath.Join(baseDir, p))
	}
	if !pathInAnyRoot(full, roots) {
		return "", false
	}
	// Resolve symlinks on the deepest existing ancestor so a symlink escaping the
	// workdir cannot bypass the lexical check.
	if resolved := evalExistingPrefix(full); resolved != "" && !pathInAnyRoot(resolved, roots) {
		return "", false
	}
	return full, true
}

func evalExistingPrefix(p string) string {
	cur := filepath.Clean(p)
	var tail []string
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			parts := append([]string{real}, tail...)
			return filepath.Clean(filepath.Join(parts...))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
}

func pathInAnyRoot(candidate string, roots []string) bool {
	for _, r := range roots {
		if isWithinRoot(candidate, r) {
			return true
		}
	}
	return false
}

func isWithinRoot(candidate, root string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

func isStrictlyWithinAnyRoot(candidate string, roots []string) bool {
	for _, r := range roots {
		rel, err := filepath.Rel(r, candidate)
		if err != nil {
			continue
		}
		if rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

func isAbsAnyOS(p string) bool {
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return true
	}
	// Windows drive path e.g. C:\ or C:/
	if len(p) >= 3 && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return true
	}
	return false
}

// isSafeInWorkdirCommand checks a shell command string for destructive operations and
// paths escaping roots.
func isSafeInWorkdirCommand(cmd, curDir string, roots []string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	// Reject fork bombs and raw backtick / subshell elevation patterns that are obviously dangerous.
	if strings.Contains(cmd, ":(){") {
		return false
	}

	segments := splitShellSegments(cmd)
	if len(segments) == 0 {
		return false
	}

	dir := curDir
	for _, seg := range segments {
		tokens := tokenizeShellSegment(seg)
		if len(tokens) == 0 {
			continue
		}
		// Skip leading env assignments (e.g. GOOS=windows go vet ./...).
		idx := 0
		for idx < len(tokens) && isEnvAssign(tokens[idx]) {
			if _, val, ok := strings.Cut(tokens[idx], "="); ok {
				if looksLikePathToken(val) {
					if _, ok := resolvePathInRoots(val, dir, roots); !ok {
						return false
					}
				}
			}
			idx++
		}
		if idx >= len(tokens) {
			continue
		}
		cmdTok := tokens[idx]
		cmdName := strings.ToLower(filepath.Base(cmdTok))
		args := tokens[idx+1:]

		switch cmdName {
		case "sudo", "su", "doas", "mkfs", "dd", "shutdown", "reboot", "halt", "poweroff":
			return false
		case "cd":
			if len(args) == 0 {
				// Bare `cd` jumps to $HOME, outside workdir.
				return false
			}
			target := args[0]
			if target == "-" {
				return false
			}
			nextDir, ok := resolvePathInRoots(target, dir, roots)
			if !ok {
				return false
			}
			dir = nextDir
			continue
		case "git":
			if !isSafeGitCommand(args, dir, roots) {
				return false
			}
		case "rm":
			if !isSafeRmCommand(args, dir, roots) {
				return false
			}
		}

		// Inspect all argument tokens (and redirections) for paths escaping roots.
		for _, tok := range args {
			if !isSafeCommandArg(tok, dir, roots) {
				return false
			}
		}
	}
	return true
}

func isSafeGitCommand(args []string, dir string, roots []string) bool {
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "-C" && i+1 < len(args) {
			if _, ok := resolvePathInRoots(args[i+1], dir, roots); !ok {
				return false
			}
			i += 2
			continue
		}
		if strings.HasPrefix(a, "-C") && len(a) > 2 {
			if _, ok := resolvePathInRoots(a[2:], dir, roots); !ok {
				return false
			}
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			i++
			continue
		}
		break
	}
	if i >= len(args) {
		return true
	}
	sub := strings.ToLower(args[i])
	if sub != "push" {
		return true
	}
	rest := args[i+1:]
	force := false
	var positional []string
	for _, a := range rest {
		switch {
		case a == "-f", a == "--force", a == "--force-with-lease", strings.HasPrefix(a, "--force-with-lease="):
			force = true
		case strings.HasPrefix(a, "-"):
			// Combined short flags like -fu
			if !strings.HasPrefix(a, "--") && strings.ContainsRune(a[1:], 'f') {
				force = true
			}
		default:
			positional = append(positional, a)
			if strings.HasPrefix(a, "+") {
				force = true
			}
		}
	}
	if !force {
		return true
	}
	// Force push with no explicit refspec (e.g. `git push --force` or `git push -f origin`)
	// could target a protected branch: escalate.
	if len(positional) < 2 {
		return false
	}
	for _, ref := range positional[1:] {
		if isProtectedBranchRef(ref) {
			return false
		}
	}
	return true
}

func isProtectedBranchRef(ref string) bool {
	ref = strings.TrimPrefix(ref, "+")
	if _, dst, ok := strings.Cut(ref, ":"); ok {
		ref = dst
	}
	ref = strings.TrimPrefix(ref, "refs/heads/")
	ref = strings.ToLower(strings.TrimSpace(ref))
	switch ref {
	case "", "main", "master", "release", "production", "prod":
		return true
	}
	return strings.HasPrefix(ref, "release/")
}

func isSafeRmCommand(args []string, dir string, roots []string) bool {
	recursive := false
	var targets []string
	endFlags := false
	for _, a := range args {
		if !endFlags && a == "--" {
			endFlags = true
			continue
		}
		if !endFlags && strings.HasPrefix(a, "-") && len(a) > 1 {
			if a == "--recursive" || (!strings.HasPrefix(a, "--") && (strings.ContainsRune(a[1:], 'r') || strings.ContainsRune(a[1:], 'R'))) {
				recursive = true
			}
			continue
		}
		targets = append(targets, a)
	}
	for _, target := range targets {
		resolved, ok := resolvePathInRoots(target, dir, roots)
		if !ok {
			return false
		}
		if recursive && !isStrictlyWithinAnyRoot(resolved, roots) {
			// Never allow `rm -rf .` on the workdir root itself.
			return false
		}
	}
	return true
}

func isSafeCommandArg(tok, dir string, roots []string) bool {
	// Strip shell redirection prefixes like >, >>, 2>, 2>>, <, &>.
	t := strings.TrimSpace(tok)
	for _, pfx := range []string{"2>>", "&>>", ">>", "2>", "&>", ">", "<"} {
		if strings.HasPrefix(t, pfx) {
			t = strings.TrimSpace(strings.TrimPrefix(t, pfx))
			break
		}
	}
	if t == "" {
		return true
	}
	// Check flag=value forms (e.g. --output=/etc/passwd).
	if strings.HasPrefix(t, "-") {
		if _, val, ok := strings.Cut(t, "="); ok && looksLikePathToken(val) {
			_, ok := resolvePathInRoots(val, dir, roots)
			return ok
		}
		return true
	}
	if !looksLikePathToken(t) {
		return true
	}
	_, ok := resolvePathInRoots(t, dir, roots)
	return ok
}

func looksLikePathToken(t string) bool {
	t = strings.Trim(strings.TrimSpace(t), `"'`)
	if t == "" {
		return false
	}
	// Allow standard null/stdio device paths.
	switch t {
	case "/dev/null", "/dev/stdout", "/dev/stderr", "/dev/stdin":
		return false
	}
	// Ignore URLs.
	for _, scheme := range []string{"http://", "https://", "git://", "ssh://", "file://"} {
		if strings.HasPrefix(t, scheme) {
			return false
		}
	}
	if t == "~" || strings.HasPrefix(t, "~/") || strings.HasPrefix(t, `~\`) {
		return true
	}
	if isAbsAnyOS(t) {
		return true
	}
	// Parent traversal segments (`..`, `../foo`, `foo/../..`).
	for _, seg := range strings.FieldsFunc(t, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}

func isEnvAssign(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := tok[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9' && i > 0) || c == '_') {
			return false
		}
	}
	return true
}

// splitShellSegments splits cmd on unquoted &&, ||, ;, |, and newlines.
func splitShellSegments(cmd string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	escaped := false
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		if quote == 0 && c == '\\' {
			cur.WriteByte(c)
			escaped = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			cur.WriteByte(c)
			continue
		}
		if c == ';' || c == '\n' {
			flush()
			continue
		}
		if (c == '&' || c == '|') && i+1 < len(cmd) && cmd[i+1] == c {
			flush()
			i++
			continue
		}
		if c == '|' {
			flush()
			continue
		}
		cur.WriteByte(c)
	}
	flush()
	return out
}

// tokenizeShellSegment splits one shell command segment into words, honoring quotes.
func tokenizeShellSegment(seg string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	escaped := false
	hasToken := false
	flush := func() {
		if hasToken || cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
			hasToken = false
		}
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			hasToken = true
			continue
		}
		if quote == 0 && c == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteByte(c)
			hasToken = true
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			hasToken = true
			continue
		}
		if c == ' ' || c == '\t' || c == '\r' {
			flush()
			continue
		}
		cur.WriteByte(c)
		hasToken = true
	}
	flush()
	return out
}
