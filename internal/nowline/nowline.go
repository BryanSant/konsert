// Package nowline turns a session's live tool label ("Run: cd /var/tmp/x && go test ./...",
// "Edit: /home/me/src/wc/wc.py", "Read: a.go · Read: b.go") into a short human "now" line
// ("running tests", "editing wc.py", "reading 2 files") for the live-work and Fleet views.
//
// It is the one place this happens: the hub fills SessionActivity.Summary with it (see
// internal/controller/activity.go) and the web app shows the summary, keeping the raw
// label as the line's hover title. It understands the label shapes every engine Konsert
// drives produces (docs/design/activity.md): claude stream-json titles ("Run: …",
// "Read: …", "Search: …"), codex items ("Run: …", "Edit: a, b") and PTY lines ("exec …",
// "Edited …"), ACP titles from grok, copilot, cursor-agent and agy/gemini, PTY session-log
// labels ("Bash: …", "view", "search_replace: …") and observed-session hooks.
package nowline

import (
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Separator joins the labels of tool calls running at the same time in one label.
const Separator = " · "

// maxLen caps a summary (runes).
const maxLen = 80

// Context is what paths are shortened against.
type Context struct {
	// Dir is the session's checkout or working directory: paths under it are shown
	// relative to it.
	Dir string
	// Home is the machine's home directory, shown as ~. Empty: guessed from Dir and the
	// paths themselves (/Users/<u>, /home/<u>, /root, C:\Users\<u>).
	Home string
}

// Summarize returns the short "now" line for a running tool: its name (tool, may be "")
// and its label. It never returns "" for a non-empty label: an unknown shape comes back
// as the label with its paths shortened.
func Summarize(tool, label string, c Context) string {
	label = strings.Join(strings.Fields(label), " ")
	tool = strings.TrimSpace(tool)
	if label == "" && tool == "" {
		return ""
	}
	if c.Home == "" {
		c.Home = guessHome(c.Dir)
	}
	parts := strings.Split(label, Separator)
	if len(parts) > 1 {
		return clip(summarizeMany(tool, parts, c))
	}
	return clip(summarizeOne(tool, label, c).text)
}

// summary is one tool call's line and what kind of work it is (for grouping calls that
// run at once).
type summary struct {
	cat   string // read | edit | write | search | shell | ...
	text  string
	files int // files named (read/edit/write)
}

func summarizeMany(tool string, parts []string, c Context) string {
	var all []summary
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			all = append(all, summarizeOne(tool, p, c))
		}
	}
	if len(all) == 0 {
		return ""
	}
	same := true
	files := 0
	for _, s := range all {
		same = same && s.cat == all[0].cat
		files += max(s.files, 1)
	}
	if same && len(all) > 1 {
		switch all[0].cat {
		case "read":
			return "reading " + plural(files, "file")
		case "edit":
			return "editing " + plural(files, "file")
		case "write":
			return "writing " + plural(files, "file")
		case "search":
			return "running " + strconv.Itoa(len(all)) + " searches"
		}
	}
	if len(all) > 1 {
		return all[len(all)-1].text + " (+" + strconv.Itoa(len(all)-1) + " more)"
	}
	return all[0].text
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// Label verbs ("Verb: arg") by category. Keys are lower case.
var colonVerbs = map[string]string{
	// shell
	"run": "shell", "bash": "shell", "shell": "shell", "powershell": "shell", "sh": "shell", "zsh": "shell",
	"exec": "shell", "execute": "shell", "terminal": "shell", "command": "shell", "cmd": "shell",
	"run_terminal_command": "shell", "run_shell_command": "shell", "run_command": "shell", "runcommand": "shell",
	"run_in_terminal": "shell", "local_shell": "shell",
	// read
	"read": "read", "view": "read", "read_file": "read", "readfile": "read", "cat": "read", "open": "read",
	"read_many_files": "read", "readmanyfiles": "read", "view_file": "read",
	// write (new files)
	"write": "write", "create": "write", "create_file": "write", "write_file": "write", "writefile": "write",
	"write_to_file": "write",
	// edit
	"edit": "edit", "multiedit": "edit", "notebookedit": "edit", "str_replace": "edit", "str_replace_editor": "edit",
	"strreplace": "edit", "search_replace": "edit", "apply_patch": "edit", "patch": "edit", "insert": "edit",
	"replace": "edit", "edit_file": "edit", "editfile": "edit", "update": "edit", "modify": "edit",
	"replace_in_file": "edit",
	// delete
	"delete": "delete", "delete_file": "delete", "remove": "delete", "rm": "delete",
	// search
	"search": "search", "grep": "search", "glob": "search", "grep_search": "search", "codebase_search": "search",
	"file_search": "search", "find": "search", "find_files": "search", "findfiles": "search", "search_text": "search",
	"searchtext": "search", "search_files": "search", "rg": "search", "ripgrep": "search",
	// list
	"list": "list", "ls": "list", "list_dir": "list", "list_directory": "list", "readfolder": "list",
	"read_folder": "list", "list_files": "list",
	// web
	"fetch": "fetch", "webfetch": "fetch", "web_fetch": "fetch", "web fetch": "fetch",
	"search the web": "websearch", "websearch": "websearch", "web_search": "websearch", "googlesearch": "websearch",
	"google_search": "websearch",
	// subagents and planning
	"agent": "agent", "task": "agent", "subagent": "agent",
}

// Space-prefixed label forms ("Edited foo.go", "Running go test", "exec bash -lc …"):
// codex and agy PTY lines, gemini/agy ACP titles, the mock engine. Checked in order.
var spaceVerbs = []struct{ prefix, cat string }{
	{"exec ", "shell"}, {"running ", "shell"}, {"executing ", "shell"}, {"shell ", "shell"}, {"run ", "shell"},
	{"$ ", "shell"}, {"bash ", "shell"},
	{"edited ", "edit"}, {"updated ", "edit"}, {"patched ", "edit"}, {"modified ", "edit"}, {"editing ", "edit"},
	{"edit ", "edit"}, {"m ", "edit"},
	{"created ", "write"}, {"wrote ", "write"}, {"write ", "write"}, {"writefile ", "write"}, {"writing ", "write"},
	{"a ", "write"},
	{"deleted ", "delete"}, {"d ", "delete"},
	{"read ", "read"}, {"readfile ", "read"}, {"readmanyfiles ", "read"}, {"reading ", "read"}, {"viewed ", "read"},
	{"view ", "read"},
	{"search ", "search"}, {"searched ", "search"}, {"searchtext ", "search"}, {"findfiles ", "search"},
	{"grep ", "search"}, {"glob ", "search"},
	{"readfolder ", "list"}, {"list ", "list"}, {"listed ", "list"},
	{"googlesearch ", "websearch"}, {"webfetch ", "fetch"}, {"fetch ", "fetch"},
}

// Tool names (and ACP kinds) by category, for labels that only name the tool.
var toolCats = map[string]string{
	"bash": "shell", "powershell": "shell", "shell": "shell", "execute": "shell", "exec": "shell", "terminal": "shell",
	"run_terminal_command": "shell", "run_shell_command": "shell", "local_shell": "shell",
	"read": "read", "view": "read", "read_file": "read", "readfile": "read", "readmanyfiles": "read",
	"edit": "edit", "multiedit": "edit", "notebookedit": "edit", "str_replace": "edit", "search_replace": "edit",
	"strreplace": "edit", "apply_patch": "edit", "patch": "edit", "insert": "edit", "str_replace_editor": "edit",
	"write": "write", "create": "write", "writefile": "write", "write_file": "write",
	"delete": "delete",
	"grep":   "search", "glob": "search", "search": "search", "grep_search": "search", "codebase_search": "search",
	"file_search": "search", "findfiles": "search", "searchtext": "search",
	"ls": "list", "list_dir": "list", "readfolder": "list",
	"webfetch": "fetch", "fetch": "fetch", "web_fetch": "fetch",
	"websearch": "websearch", "web_search": "websearch", "googlesearch": "websearch",
	"task": "agent", "agent": "agent",
	"todowrite": "plan", "think": "plan", "plan": "plan", "update_plan": "plan", "todo_write": "plan",
}

var mcpTitleRe = regexp.MustCompile(`^([a-z][a-z0-9_.-]*): ([A-Za-z][A-Za-z0-9_.-]*)$`)

func summarizeOne(tool, label string, c Context) summary {
	lower := strings.ToLower(label)
	tl := strings.ToLower(tool)
	if strings.HasPrefix(tl, "mcp__") {
		tl = ""
	}
	switch {
	case lower == "update plan" || lower == "update todos" || lower == "todo list" || toolCats[lower] == "plan":
		return summary{cat: "plan", text: "updating the plan"}
	case lower == "edit files":
		return summary{cat: "edit", text: "editing files"}
	}
	// A whole label in backticks is a command (some ACP agents title shell calls so).
	if len(label) > 2 && label[0] == '`' && label[len(label)-1] == '`' {
		return phrase("shell", strings.Trim(label, "`"), c)
	}
	// "Verb: arg"
	if verb, arg, ok := strings.Cut(label, ": "); ok {
		if cat, known := colonVerbs[strings.ToLower(strings.TrimSpace(verb))]; known {
			return phrase(cat, arg, c)
		}
		if m := mcpTitleRe.FindStringSubmatch(label); m != nil {
			return summary{cat: "mcp", text: strings.ReplaceAll(m[2], "_", " ") + " (" + m[1] + ")"}
		}
	}
	for _, v := range spaceVerbs {
		if strings.HasPrefix(lower, v.prefix) && len(label) > len(v.prefix) {
			rest := label[len(v.prefix):]
			if v.cat == "shell" && isProse(rest) {
				// "Run the test suite": an agent's title, not a command.
				return summary{cat: "shell", text: lowerFirst(shortenPaths(label, c))}
			}
			return phrase(v.cat, rest, c)
		}
	}
	// The label is just the tool's name (session logs without input, process reports).
	if cat, ok := toolCats[lower]; ok {
		return bare(cat)
	}
	// An ACP kind or a known tool name says what the (free-form) label is.
	if cat, ok := toolCats[tl]; ok && label != "" {
		if cat == "shell" || cat == "search" || cat == "websearch" || cat == "agent" {
			if looksLikeTitle(label) {
				return summary{cat: cat, text: lowerFirst(shortenPaths(label, c))}
			}
		}
		return phrase(cat, label, c)
	}
	if label == "" {
		if cat, ok := toolCats[tl]; ok {
			return bare(cat)
		}
		return summary{cat: "other", text: "using " + tool}
	}
	return summary{cat: "other", text: shortenPaths(label, c)}
}

// looksLikeTitle: an agent's own prose title ("Run the test suite") rather than a command.
func looksLikeTitle(s string) bool {
	f := strings.Fields(s)
	if len(f) == 0 {
		return false
	}
	r, _ := utf8.DecodeRuneInString(f[0])
	return len(f) >= 2 && r >= 'A' && r <= 'Z' && !strings.ContainsAny(s, "/=|&;$<>`")
}

// proseWords start English titles, never commands.
var proseWords = map[string]bool{"the": true, "a": true, "an": true, "all": true, "some": true, "this": true, "these": true, "my": true, "our": true, "unit": true, "tests": true}

// isProse: the words after "Run "/"Running " read as English, not as a command line.
func isProse(rest string) bool {
	f := strings.Fields(rest)
	if len(f) < 2 || strings.ContainsAny(rest, "/=|&;$<>`-") {
		return false
	}
	if proseWords[strings.ToLower(f[0])] {
		return true
	}
	prio, _ := classify(strip(tokenize(rest)), Context{})
	return prio <= prioOther && len(f) >= 3
}

func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if r >= 'A' && r <= 'Z' && n < len(s) {
		// Keep acronyms ("NPM", "README") as they are.
		next, _ := utf8.DecodeRuneInString(s[n:])
		if next >= 'A' && next <= 'Z' {
			return s
		}
		return string(r+('a'-'A')) + s[n:]
	}
	return s
}

func bare(cat string) summary {
	switch cat {
	case "shell":
		return summary{cat: cat, text: "running a command"}
	case "read":
		return summary{cat: cat, text: "reading a file"}
	case "edit":
		return summary{cat: cat, text: "editing a file"}
	case "write":
		return summary{cat: cat, text: "writing a file"}
	case "delete":
		return summary{cat: cat, text: "deleting a file"}
	case "search":
		return summary{cat: cat, text: "searching the code"}
	case "list":
		return summary{cat: cat, text: "listing files"}
	case "fetch":
		return summary{cat: cat, text: "fetching a web page"}
	case "websearch":
		return summary{cat: cat, text: "searching the web"}
	case "agent":
		return summary{cat: cat, text: "running a subagent"}
	case "plan":
		return summary{cat: cat, text: "updating the plan"}
	}
	return summary{cat: "other", text: "working"}
}

// phrase builds the line for a category and its argument (a command, path(s), pattern…).
func phrase(cat, arg string, c Context) summary {
	arg = trimEllipsis(strings.TrimSpace(arg))
	if arg == "" {
		return bare(cat)
	}
	switch cat {
	case "shell":
		return Command(arg, c)
	case "read", "edit", "write", "delete":
		files := splitPaths(arg)
		verb := map[string]string{"read": "reading", "edit": "editing", "write": "writing", "delete": "deleting"}[cat]
		if len(files) > 1 {
			return summary{cat: cat, text: verb + " " + plural(len(files), "file"), files: len(files)}
		}
		return summary{cat: cat, text: verb + " " + ShortPath(files[0], c), files: 1}
	case "search":
		return summary{cat: cat, text: "searching for " + quoteArg(arg)}
	case "list":
		return summary{cat: cat, text: "listing " + ShortPath(unquote(arg), c)}
	case "fetch":
		return summary{cat: cat, text: "fetching " + host(arg)}
	case "websearch":
		return summary{cat: cat, text: "searching the web for " + quoteArg(arg)}
	case "agent":
		return summary{cat: cat, text: "subagent: " + lowerFirst(arg)}
	}
	return summary{cat: cat, text: shortenPaths(arg, c)}
}

// splitPaths splits "a.go, b.go" (codex file changes) into paths.
func splitPaths(arg string) []string {
	var out []string
	for p := range strings.SplitSeq(arg, ", ") {
		if p = unquote(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{arg}
	}
	return out
}

// trimEllipsis drops a truncation marker ("…", or "..." not part of a path like ./...).
func trimEllipsis(s string) string {
	s = strings.TrimSuffix(s, "…")
	if strings.HasSuffix(s, "...") && !strings.HasSuffix(s, "/...") && !strings.HasSuffix(s, " ...") && s != "..." {
		s = strings.TrimSuffix(s, "...")
	}
	return strings.TrimSpace(s)
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'' || s[0] == '`') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// quoteArg shows a search pattern or query: unquoted, capped.
func quoteArg(s string) string {
	s = unquote(strings.TrimSpace(s))
	if utf8.RuneCountInString(s) > 40 {
		r := []rune(s)
		s = string(r[:39]) + "…"
	}
	return s
}

func host(raw string) string {
	raw = strings.TrimSpace(raw)
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return strings.TrimPrefix(u.Host, "www.")
	}
	return quoteArg(raw)
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	r := []rune(s)
	return string(r[:maxLen-1]) + "…"
}

// --- paths ---------------------------------------------------------------------------

var homeRe = regexp.MustCompile(`^(/Users/[^/]+|/home/[^/]+|/root|[A-Za-z]:[\\/]Users[\\/][^\\/]+)`)

func guessHome(dir string) string {
	if m := homeRe.FindString(dir); m != "" {
		return m
	}
	return ""
}

// slash normalizes separators and macOS's /private alias for comparisons.
func slash(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if strings.HasPrefix(p, "/private/") {
		p = strings.TrimPrefix(p, "/private")
	}
	return strings.TrimRight(p, "/")
}

// ShortPath shows p relative to the session's directory, else with ~ for the home
// directory, else as it is.
func ShortPath(p string, c Context) string {
	p = unquote(strings.TrimSpace(p))
	if p == "" || p == "." || p == "./" {
		return p
	}
	if rel := strings.TrimPrefix(p, "./"); rel != p {
		return rel
	}
	sp := slash(p)
	if c.Dir != "" {
		d := slash(c.Dir)
		if d != "" && strings.EqualFold(sp, d) {
			return "."
		}
		if d != "" && len(sp) > len(d)+1 && sp[len(d)] == '/' && strings.EqualFold(sp[:len(d)], d) {
			return sp[len(d)+1:]
		}
	}
	home := c.Home
	if home == "" {
		home = homeRe.FindString(p)
	}
	if home != "" {
		h := slash(home)
		if strings.EqualFold(sp, h) {
			return "~"
		}
		if len(sp) > len(h)+1 && sp[len(h)] == '/' && strings.EqualFold(sp[:len(h)], h) {
			return "~/" + sp[len(h)+1:]
		}
	}
	return p
}

var absPathRe = regexp.MustCompile(`(?:[A-Za-z]:\\|/)[^\s'"` + "`" + `;|&<>()]+`)

// shortenPaths shortens every absolute path inside free text.
func shortenPaths(s string, c Context) string {
	if c.Home == "" {
		c.Home = guessHome(c.Dir)
	}
	return absPathRe.ReplaceAllStringFunc(s, func(p string) string {
		// Leave URLs alone ("https://x/y" matches from "//x/y").
		return ShortPath(p, c)
	})
}

// baseName is the last element of a slash or backslash path.
func baseName(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	return path.Base(p)
}
