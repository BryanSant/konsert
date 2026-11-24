package nowline

import (
	"strings"
)

// Shell command lines: "cd /x && CI=1 npm test -- --watch=false | tail -20" → "running
// tests". The command is split at && || ; | and newlines; setup steps (cd, export, env
// assignments, source …) are dropped; the remaining steps are recognised across
// ecosystems and the most telling one wins (tests over a build over installs …).

// Priorities of recognised steps.
const (
	prioNone    = 0
	prioEcho    = 5
	prioOther   = 10
	prioFS      = 20
	prioRead    = 30
	prioGit     = 35
	prioSearch  = 40
	prioGH      = 45
	prioRun     = 50
	prioLint    = 60
	prioGitPush = 65
	prioInstall = 70
	prioBuild   = 80
	prioTest    = 90
)

// Command summarises a shell command line.
func Command(cmd string, c Context) summary {
	if c.Home == "" {
		c.Home = guessHome(c.Dir)
	}
	cmd = trimEllipsis(strings.TrimSpace(cmd))
	best := summary{cat: "shell"}
	bestPrio := -1
	var last []string
	for _, seg := range segments(cmd) {
		toks := strip(tokenize(seg))
		if len(toks) == 0 {
			continue
		}
		last = toks
		if isSetup(toks) {
			continue
		}
		prio, text := classify(toks, c)
		if prio > bestPrio {
			best.text, bestPrio = text, prio
		}
	}
	if bestPrio < 0 {
		if last == nil {
			return bare("shell")
		}
		_, best.text = classify(last, c)
	}
	return best
}

// segments splits a command line into its pipeline heads: the first command of every
// && / || / ; / & / newline-separated step (filters after a | are dropped). Quotes are
// respected.
func segments(cmd string) []string {
	var out []string
	var cur strings.Builder
	inPipe := false
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" && !inPipe {
			out = append(out, s)
		}
		cur.Reset()
	}
	var quote byte
	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]
		if quote != 0 {
			if ch == '\\' && quote == '"' && i+1 < len(cmd) {
				cur.WriteByte(ch)
				i++
				cur.WriteByte(cmd[i])
				continue
			}
			if ch == quote {
				quote = 0
			}
			cur.WriteByte(ch)
			continue
		}
		switch ch {
		case '\'', '"', '`':
			quote = ch
			cur.WriteByte(ch)
		case '\\':
			cur.WriteByte(ch)
			if i+1 < len(cmd) {
				i++
				cur.WriteByte(cmd[i])
			}
		case '&', '|':
			double := i+1 < len(cmd) && cmd[i+1] == ch
			if ch == '&' && !double && (i > 0 && (cmd[i-1] == '>' || cmd[i-1] == '<') || i+1 < len(cmd) && cmd[i+1] == '>') {
				cur.WriteByte(ch) // 2>&1, &>file
				continue
			}
			flush()
			if double {
				i++
				inPipe = false
			} else {
				inPipe = ch == '|'
			}
		case ';', '\n':
			flush()
			inPipe = false
		default:
			cur.WriteByte(ch)
		}
	}
	flush()
	return out
}

// tokenize splits one command into words, removing quotes; redirections are dropped.
func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	has := false
	var quote byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
				continue
			}
			if ch == '\\' && quote == '"' && i+1 < len(s) {
				i++
				ch = s[i]
			}
			cur.WriteByte(ch)
			continue
		}
		switch ch {
		case '\'', '"':
			quote, has = ch, true
		case '\\':
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				has = true
			}
		case ' ', '\t':
			if has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(ch)
			has = true
		}
	}
	if has {
		out = append(out, cur.String())
	}
	// Drop redirections (> out.txt, 2>&1, < in).
	kept := out[:0]
	for i := 0; i < len(out); i++ {
		t := out[i]
		switch {
		case t == ">" || t == ">>" || t == "<" || t == "2>" || t == "&>" || t == "2>>":
			i++ // and its target
		case strings.HasPrefix(t, ">") || strings.HasPrefix(t, "2>") || strings.HasPrefix(t, "&>") ||
			strings.HasPrefix(t, "1>") || (strings.HasPrefix(t, "<") && t != "<"):
		default:
			kept = append(kept, t)
		}
	}
	return kept
}

// strip removes leading environment assignments and wrappers that don't change what a
// step does (sudo, time, env, timeout N, npx, uv run, bundle exec, python -m …), and
// unwraps "bash -lc '…'".
func strip(t []string) []string {
	for len(t) > 0 {
		p := prog(t[0])
		switch {
		case isAssignment(t[0]):
			t = t[1:]
		case p == "env":
			t = t[1:]
			for len(t) > 0 && (isAssignment(t[0]) || strings.HasPrefix(t[0], "-")) {
				if t[0] == "-u" && len(t) > 1 {
					t = t[1:]
				}
				t = t[1:]
			}
		case p == "sudo" || p == "time" || p == "nice" || p == "nohup" || p == "command" || p == "exec" ||
			p == "caffeinate" || p == "xvfb-run" || p == "stdbuf" || p == "unbuffer" || p == "ionice":
			t = t[1:]
			for len(t) > 0 && strings.HasPrefix(t[0], "-") {
				t = t[1:]
			}
		case p == "timeout" || p == "gtimeout":
			t = t[1:]
			for len(t) > 0 && strings.HasPrefix(t[0], "-") {
				t = t[1:]
			}
			if len(t) > 0 {
				t = t[1:] // the duration
			}
		case p == "npx" || p == "bunx" || p == "pnpx":
			t = t[1:]
			for len(t) > 0 && strings.HasPrefix(t[0], "-") {
				t = t[1:]
			}
		case (p == "pnpm" || p == "yarn") && len(t) > 1 && (t[1] == "dlx" || t[1] == "exec"):
			t = t[2:]
		case (p == "uv" || p == "poetry" || p == "pipenv" || p == "pdm" || p == "hatch" || p == "rye") && len(t) > 1 && t[1] == "run":
			t = t[2:]
			for len(t) > 0 && strings.HasPrefix(t[0], "-") {
				t = t[1:]
			}
		case p == "uvx" || p == "pipx" && len(t) > 1 && t[1] == "run":
			if p == "pipx" {
				t = t[1:]
			}
			t = t[1:]
		case p == "bundle" && len(t) > 1 && t[1] == "exec":
			t = t[2:]
		case isPython(p) && len(t) > 2 && t[1] == "-m":
			t = t[2:]
		case isShell(p):
			if inner, ok := shellScript(t); ok {
				// Summarise the script's own steps.
				return strip(tokenize(firstStep(inner)))
			}
			return t
		default:
			return t
		}
	}
	return t
}

// firstStep returns the most telling step of a script (for "bash -lc 'cd x && make'").
func firstStep(script string) string {
	segs := segments(script)
	for _, s := range segs {
		if toks := strip(tokenize(s)); len(toks) > 0 && !isSetup(toks) {
			return s
		}
	}
	if len(segs) > 0 {
		return segs[len(segs)-1]
	}
	return script
}

func isShell(p string) bool {
	switch p {
	case "bash", "sh", "zsh", "dash", "fish", "pwsh", "powershell", "cmd":
		return true
	}
	return false
}

// shellScript returns the script of "bash -lc '…'", "pwsh -Command …", "cmd /c …".
func shellScript(t []string) (string, bool) {
	for i := 1; i < len(t); i++ {
		a := strings.ToLower(t[i])
		if a == "-c" || a == "-lc" || a == "-ic" || a == "-cl" || a == "-command" || a == "/c" {
			if i+1 < len(t) {
				return strings.Join(t[i+1:], " "), true
			}
		}
	}
	return "", false
}

func isAssignment(s string) bool {
	eq := strings.IndexByte(s, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		ch := s[i]
		if !(ch == '_' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || i > 0 && ch >= '0' && ch <= '9') {
			return false
		}
	}
	return true
}

func isPython(p string) bool {
	return p == "python" || p == "python3" || p == "py" || strings.HasPrefix(p, "python3.")
}

// prog is a command word's program name: base name, lower case, without .exe/.cmd.
func prog(s string) string {
	b := strings.ToLower(baseName(s))
	for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		b = strings.TrimSuffix(b, ext)
	}
	return b
}

// isSetup reports steps that only prepare the environment.
func isSetup(t []string) bool {
	switch prog(t[0]) {
	case "cd", "pushd", "popd", "export", "set", "setopt", "source", ".", "true", ":", "unset", "alias",
		"ulimit", "umask", "clear", "shopt", "trap", "eval", "nvm", "pyenv", "conda", "rbenv", "chcp", "set-location":
		return prog(t[0]) != "conda" || len(t) > 1 && (t[1] == "activate" || t[1] == "deactivate")
	}
	return false
}

// args returns the positional (non-flag) arguments after skip words.
func positional(t []string) []string {
	var out []string
	for _, a := range t {
		if strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}

func at(t []string, i int) string {
	if i < len(t) {
		return t[i]
	}
	return ""
}

// testScope names what a test run is limited to, when that is a file or directory.
func testScope(args []string, c Context) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			continue
		}
		if strings.HasPrefix(a, "-") {
			// Flags that take a value.
			switch a {
			case "-run", "-k", "-m", "-p", "-t", "--grep", "-g", "--filter", "-timeout", "-count", "--project", "-c", "--config", "-n", "--testNamePattern":
				i++
			}
			continue
		}
		if a == "./..." || a == "." || a == "..." || a == "test" || a == "tests" {
			continue
		}
		if strings.ContainsAny(a, "/\\") || strings.Contains(a, ".") {
			scope := strings.TrimSuffix(ShortPath(strings.TrimSuffix(a, "/..."), c), "/")
			if scope != "" && scope != "." {
				return " in " + scope
			}
		}
	}
	return ""
}

func gitSub(t []string) (string, []string) {
	for i := 1; i < len(t); i++ {
		a := t[i]
		switch {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a, t[i+1:]
		}
	}
	return "", nil
}

// classify recognises one step: its priority and line.
func classify(t []string, c Context) (int, string) {
	p := prog(t[0])
	rest := t[1:]
	pos := positional(rest)
	sub := at(pos, 0)
	switch p {
	// --- Go
	case "go":
		switch sub {
		case "test":
			return prioTest, "running tests" + testScope(rest[1:], c)
		case "build", "install":
			return prioBuild, "building"
		case "vet":
			return prioLint, "running go vet"
		case "mod":
			switch at(pos, 1) {
			case "tidy":
				return prioInstall, "tidying go modules"
			case "download", "vendor":
				return prioInstall, "installing deps"
			}
			return prioOther, "go mod " + at(pos, 1)
		case "get":
			return prioInstall, "installing deps"
		case "run":
			return prioRun, "running " + runTarget(pos[1:], c, "go program")
		case "generate":
			return prioBuild, "generating code"
		case "fmt":
			return prioLint, "formatting"
		}
		return prioOther, strings.TrimSpace("go " + sub)
	case "gofmt", "goimports", "gofumpt":
		return prioLint, "formatting"
	case "staticcheck", "golangci-lint", "revive", "govulncheck":
		return prioLint, "linting (" + p + ")"

	// --- JavaScript / TypeScript
	case "npm", "pnpm", "yarn", "bun":
		return classifyNode(p, rest, pos, c)
	case "node", "deno", "tsx", "ts-node":
		if p == "deno" && sub == "test" {
			return prioTest, "running tests"
		}
		if p == "deno" && (sub == "lint" || sub == "fmt") {
			return prioLint, map[string]string{"lint": "linting", "fmt": "formatting"}[sub]
		}
		if p == "deno" && sub == "run" {
			pos = pos[1:]
		}
		return prioRun, "running " + runTarget(pos, c, p)
	case "jest", "vitest", "mocha", "ava", "karma", "tap", "jasmine", "cypress", "wdio":
		return prioTest, "running tests" + testScope(rest, c)
	case "playwright":
		if sub == "test" {
			return prioTest, "running e2e tests"
		}
		if sub == "install" {
			return prioInstall, "installing browsers"
		}
		return prioOther, "playwright " + sub
	case "ng":
		switch sub {
		case "test":
			return prioTest, "running tests"
		case "e2e":
			return prioTest, "running e2e tests"
		case "build":
			return prioBuild, "building"
		case "serve":
			return prioRun, "starting the dev server"
		case "lint":
			return prioLint, "linting"
		}
		return prioOther, "ng " + sub
	case "tsc", "vue-tsc":
		for _, a := range rest {
			if a == "--noEmit" || a == "--noemit" {
				return prioLint, "type-checking"
			}
		}
		return prioBuild, "building"
	case "vite", "next", "nuxt", "astro", "remix", "svelte-kit":
		if sub == "build" {
			return prioBuild, "building"
		}
		return prioRun, "starting the dev server"
	case "webpack", "esbuild", "rollup", "parcel", "turbo", "nx", "swc":
		if sub == "test" || (p == "turbo" || p == "nx") && strings.Contains(sub, "test") {
			return prioTest, "running tests"
		}
		return prioBuild, "building"
	case "eslint", "biome", "oxlint", "stylelint", "tslint", "standard", "xo":
		if p == "biome" && sub == "format" {
			return prioLint, "formatting"
		}
		return prioLint, "linting"
	case "prettier", "dprint":
		return prioLint, "formatting"

	// --- Python
	case "python", "python3", "py":
		if len(pos) == 0 || at(rest, 0) == "-c" {
			return prioRun, "running python"
		}
		return prioRun, "running " + runTarget(pos, c, "python")
	case "pytest", "py.test", "unittest", "nosetests", "nose2", "tox", "nox", "ward", "behave":
		return prioTest, "running tests" + testScope(rest, c)
	case "pip", "pip3", "pipx":
		if sub == "install" || sub == "download" || sub == "sync" {
			return prioInstall, "installing deps"
		}
		return prioOther, p + " " + sub
	case "uv", "poetry", "pdm", "pipenv", "hatch", "rye", "conda", "mamba", "micromamba":
		switch sub {
		case "sync", "install", "add", "lock", "update", "upgrade", "i", "create", "env":
			return prioInstall, "installing deps"
		case "pip":
			if at(pos, 1) == "install" || at(pos, 1) == "sync" {
				return prioInstall, "installing deps"
			}
		case "build":
			return prioBuild, "building"
		case "test":
			return prioTest, "running tests"
		}
		return prioOther, strings.TrimSpace(p + " " + sub)
	case "ruff":
		if sub == "format" {
			return prioLint, "formatting"
		}
		return prioLint, "linting"
	case "black", "isort", "autopep8", "yapf":
		return prioLint, "formatting"
	case "flake8", "pylint", "pyflakes", "bandit", "pycodestyle":
		return prioLint, "linting"
	case "mypy", "pyright", "pytype", "basedpyright":
		return prioLint, "type-checking"

	// --- Rust
	case "cargo":
		switch sub {
		case "test", "t", "nextest":
			return prioTest, "running tests"
		case "build", "b", "check", "c":
			return prioBuild, "building"
		case "run", "r":
			return prioRun, "running the program"
		case "add", "fetch", "install", "update":
			return prioInstall, "installing deps"
		case "clippy":
			return prioLint, "linting (clippy)"
		case "fmt":
			return prioLint, "formatting"
		case "bench":
			return prioTest, "running benchmarks"
		}
		return prioOther, "cargo " + sub
	case "rustc", "gcc", "g++", "clang", "clang++", "cc", "c++", "javac", "kotlinc", "swiftc", "zig", "nim", "ghc", "csc", "scalac":
		if p == "zig" && sub == "test" || p == "zig" && sub == "build" && at(pos, 1) == "test" {
			return prioTest, "running tests"
		}
		return prioBuild, "building"
	case "rustfmt", "clang-format":
		return prioLint, "formatting"

	// --- Build systems
	case "make", "gmake", "just", "task", "mage", "ninja", "rake", "invoke", "mise":
		if p == "mise" && sub == "run" {
			pos = pos[1:]
			sub = at(pos, 0)
		}
		if p == "mise" && (sub == "install" || sub == "use") {
			return prioInstall, "installing tools"
		}
		return classifyTarget(p, sub)
	case "cmake":
		for _, a := range rest {
			if a == "--build" {
				return prioBuild, "building"
			}
		}
		return prioBuild, "configuring the build"
	case "ctest":
		return prioTest, "running tests"
	case "meson":
		if sub == "test" {
			return prioTest, "running tests"
		}
		return prioBuild, "building"
	case "bazel", "bazelisk", "buck", "buck2", "pants":
		if sub == "test" {
			return prioTest, "running tests"
		}
		if sub == "run" {
			return prioRun, "running " + runTarget(pos[1:], c, "a target")
		}
		return prioBuild, "building"

	// --- JVM, .NET, Apple, others
	case "mvn", "mvnw", "gradle", "gradlew", "sbt", "lein", "ant", "kotlin":
		return classifyJVM(p, pos)
	case "dotnet":
		switch sub {
		case "test":
			return prioTest, "running tests"
		case "build", "publish", "pack", "msbuild":
			return prioBuild, "building"
		case "restore", "add", "tool":
			return prioInstall, "installing deps"
		case "run", "watch":
			return prioRun, "running the program"
		case "format":
			return prioLint, "formatting"
		}
		return prioOther, "dotnet " + sub
	case "msbuild":
		return prioBuild, "building"
	case "swift":
		switch sub {
		case "test":
			return prioTest, "running tests"
		case "build":
			return prioBuild, "building"
		case "run":
			return prioRun, "running the program"
		case "package":
			return prioInstall, "resolving packages"
		}
		return prioOther, "swift " + sub
	case "xcodebuild":
		for _, a := range rest {
			if a == "test" || a == "test-without-building" {
				return prioTest, "running tests"
			}
		}
		return prioBuild, "building"
	case "swiftlint", "ktlint", "detekt", "rubocop", "shellcheck", "hadolint", "markdownlint", "yamllint", "actionlint", "cppcheck", "clang-tidy", "credo", "phpstan", "psalm":
		return prioLint, "linting"
	case "swiftformat", "shfmt", "terraform-fmt":
		return prioLint, "formatting"
	case "mix":
		switch sub {
		case "test":
			return prioTest, "running tests"
		case "deps.get", "deps.update":
			return prioInstall, "installing deps"
		case "compile":
			return prioBuild, "building"
		case "format":
			return prioLint, "formatting"
		}
		return prioOther, "mix " + sub
	case "rspec", "minitest", "cucumber", "phpunit", "pest", "codecept", "busted", "prove", "bats", "elm-test", "dart-test":
		return prioTest, "running tests" + testScope(rest, c)
	case "bundle", "bundler":
		if sub == "" || sub == "install" || sub == "update" || sub == "add" {
			return prioInstall, "installing deps"
		}
		return prioOther, "bundle " + sub
	case "gem", "composer", "cpan", "cpanm", "luarocks", "opam", "cabal", "stack", "pub", "dart", "flutter", "elm", "nimble", "vcpkg", "conan":
		switch sub {
		case "install", "require", "add", "get", "update", "i", "deps", "restore", "upgrade":
			return prioInstall, "installing deps"
		case "test":
			return prioTest, "running tests"
		case "build", "compile":
			return prioBuild, "building"
		case "run":
			return prioRun, "running the program"
		case "analyze", "format":
			return prioLint, map[string]string{"analyze": "analyzing", "format": "formatting"}[sub]
		}
		return prioOther, strings.TrimSpace(p + " " + sub)
	case "brew", "apt", "apt-get", "yum", "dnf", "pacman", "apk", "zypper", "port", "choco", "winget", "scoop", "pod", "snap", "nix-env":
		switch sub {
		case "install", "add", "-S", "upgrade", "update", "reinstall", "i":
			return prioInstall, "installing deps"
		}
		if p == "pacman" && strings.HasPrefix(at(rest, 0), "-S") {
			return prioInstall, "installing deps"
		}
		return prioOther, strings.TrimSpace(p + " " + sub)

	// --- git and GitHub
	case "git":
		s, srest := gitSub(t)
		switch s {
		case "commit", "push", "merge", "rebase", "cherry-pick", "revert", "tag", "am":
			return prioGitPush, "git " + s
		case "grep":
			return prioSearch, "searching for " + quoteArg(at(positional(srest), 0))
		case "":
			return prioGit, "git"
		}
		return prioGit, "git " + s
	case "gh", "glab", "hub":
		words := []string{p}
		for _, a := range pos {
			if len(words) == 3 || strings.ContainsAny(a, "/.:#") {
				break
			}
			words = append(words, a)
		}
		return prioGH, strings.Join(words, " ")

	// --- search, read, list
	case "grep", "egrep", "fgrep", "rg", "ag", "ack", "ugrep", "findstr", "select-string":
		return prioSearch, "searching for " + quoteArg(grepPattern(rest))
	case "find", "fd", "fdfind", "locate", "get-childitem", "gci":
		for i, a := range rest {
			if (a == "-name" || a == "-iname" || a == "-path" || a == "-filter") && i+1 < len(rest) {
				return prioSearch, "finding " + quoteArg(rest[i+1])
			}
		}
		if (p == "fd" || p == "fdfind") && sub != "" {
			return prioSearch, "finding " + quoteArg(sub)
		}
		if sub != "" {
			return prioSearch, "finding files in " + ShortPath(sub, c)
		}
		return prioSearch, "finding files"
	case "cat", "head", "tail", "less", "more", "bat", "nl", "get-content", "type", "view", "xxd", "hexdump", "od", "strings", "file", "stat":
		return readLine(pos, c)
	case "sed", "awk", "perl":
		for _, a := range rest {
			if a == "-i" || strings.HasPrefix(a, "-i") && p != "awk" {
				files := filesOnly(pos[1:])
				if len(files) == 1 {
					return prioRead, "editing " + ShortPath(files[0], c)
				}
				return prioRead, "editing files"
			}
		}
		if len(pos) > 1 {
			return readLine(pos[1:], c)
		}
		return prioOther, "running " + p
	case "wc":
		if len(pos) == 1 {
			return prioRead, "counting lines in " + ShortPath(pos[0], c)
		}
		return prioRead, "counting lines"
	case "ls", "tree", "dir", "exa", "eza", "lsd":
		if sub == "" || strings.Trim(sub, "0123456789") == "" {
			return prioRead, "listing files"
		}
		return prioRead, "listing " + ShortPath(sub, c)
	case "diff", "cmp", "colordiff", "delta":
		return prioRead, "comparing files"
	case "jq", "yq":
		if len(pos) > 1 {
			return prioRead, "reading " + ShortPath(pos[len(pos)-1], c)
		}
		return prioOther, "running " + p

	// --- files
	case "mkdir", "md", "new-item":
		return prioFS, "creating " + ShortPath(at(pos, 0), c)
	case "touch":
		return prioFS, "creating " + ShortPath(at(pos, 0), c)
	case "rm", "rmdir", "del", "rd", "remove-item", "unlink", "trash":
		return prioFS, "removing " + many(pos, c)
	case "mv", "move", "ren", "rename", "move-item":
		return prioFS, "moving " + ShortPath(at(pos, 0), c)
	case "cp", "copy", "rsync", "scp", "copy-item", "xcopy", "robocopy":
		return prioFS, "copying " + ShortPath(at(pos, 0), c)
	case "chmod", "chown", "chgrp", "icacls":
		return prioFS, "changing permissions"
	case "ln":
		return prioFS, "linking " + ShortPath(at(pos, 0), c)
	case "tar", "zip", "unzip", "gzip", "gunzip", "7z", "xz":
		return prioFS, "unpacking archives"

	// --- network, containers, processes
	case "curl", "wget", "http", "https", "xh", "invoke-webrequest", "iwr", "invoke-restmethod", "irm":
		for _, a := range pos {
			if strings.Contains(a, "://") || strings.Contains(a, ".") {
				return prioRun, "fetching " + host(a)
			}
		}
		return prioRun, "fetching a URL"
	case "docker", "podman", "nerdctl", "docker-compose":
		if p == "docker-compose" || sub == "compose" {
			for _, a := range pos {
				switch a {
				case "up", "start":
					return prioRun, "starting containers"
				case "down", "stop":
					return prioRun, "stopping containers"
				case "build":
					return prioBuild, "building images"
				}
			}
			return prioRun, "docker compose"
		}
		switch sub {
		case "build", "buildx":
			return prioBuild, "building an image"
		case "run":
			return prioRun, "running a container"
		case "pull":
			return prioInstall, "pulling images"
		}
		return prioOther, strings.TrimSpace(p + " " + sub)
	case "kubectl", "helm", "terraform", "tofu", "pulumi", "ansible-playbook", "aws", "gcloud", "az", "fly", "flyctl", "vercel", "wrangler":
		return prioRun, strings.TrimSpace(p + " " + sub)
	case "sleep", "start-sleep", "wait":
		return prioEcho, "waiting"
	case "kill", "pkill", "killall", "taskkill", "stop-process":
		return prioFS, "stopping processes"
	case "ps", "pgrep", "top", "htop", "lsof", "netstat", "ss":
		return prioRead, "checking processes"
	case "echo", "printf", "write-output", "write-host", "say":
		return prioEcho, "printing"
	case "which", "where", "whereis":
		return prioRead, "looking up " + at(pos, 0)
	case "open", "xdg-open", "start":
		return prioRun, "opening " + ShortPath(at(pos, 0), c)
	}
	// ./script.sh, bin/tool, /usr/local/bin/x
	if strings.ContainsAny(t[0], "/\\") {
		return prioRun, "running " + ShortPath(t[0], c)
	}
	return prioOther, "running " + p
}

func classifyNode(p string, rest, pos []string, c Context) (int, string) {
	sub := at(pos, 0)
	script := sub
	if sub == "run" || sub == "run-script" {
		script = at(pos, 1)
	}
	lowerScript := strings.ToLower(script)
	switch {
	case sub == "test" || sub == "t" || sub == "tst" || strings.HasPrefix(lowerScript, "test") || strings.HasSuffix(lowerScript, ":test"):
		if strings.Contains(lowerScript, "e2e") {
			return prioTest, "running e2e tests"
		}
		return prioTest, "running tests"
	case strings.Contains(lowerScript, "e2e"):
		return prioTest, "running e2e tests"
	case sub == "install" || sub == "i" || sub == "ci" || sub == "add" || sub == "update" || sub == "upgrade" || sub == "up" ||
		(p == "yarn" && sub == "") || (p == "bun" && sub == "") || sub == "rebuild":
		return prioInstall, "installing deps"
	case strings.HasPrefix(lowerScript, "build") || strings.HasSuffix(lowerScript, ":build"):
		return prioBuild, "building"
	case strings.HasPrefix(lowerScript, "lint"):
		return prioLint, "linting"
	case strings.HasPrefix(lowerScript, "typecheck") || strings.HasPrefix(lowerScript, "type-check") || lowerScript == "tsc" || lowerScript == "check":
		return prioLint, "type-checking"
	case strings.HasPrefix(lowerScript, "format") || lowerScript == "fmt" || lowerScript == "prettier":
		return prioLint, "formatting"
	case lowerScript == "start" || lowerScript == "dev" || lowerScript == "serve" || lowerScript == "preview" || lowerScript == "watch":
		return prioRun, "starting the dev server"
	case sub == "exec" || sub == "x":
		if len(pos) > 1 {
			return classify(strip(pos[1:]), c)
		}
	case p == "bun" && (strings.HasSuffix(script, ".ts") || strings.HasSuffix(script, ".js")):
		return prioRun, "running " + ShortPath(script, c)
	case sub == "run" && script != "":
		return prioRun, p + " run " + script
	case script == "":
		return prioOther, p
	}
	if p == "pnpm" || p == "yarn" || p == "bun" {
		// "pnpm lint", "yarn build": scripts without run.
		return prioRun, p + " " + script
	}
	return prioOther, strings.TrimSpace(p + " " + sub)
}

// classifyTarget handles make-like tools by their target.
func classifyTarget(p, target string) (int, string) {
	lt := strings.ToLower(target)
	switch {
	case lt == "" || lt == "all" || lt == "build" || strings.HasPrefix(lt, "build-") || lt == "release" || lt == "dist" || lt == "compile":
		return prioBuild, "building"
	case strings.HasPrefix(lt, "test") || lt == "check" || lt == "spec" || lt == "e2e" || strings.HasSuffix(lt, "-test") || strings.HasSuffix(lt, "_test"):
		if strings.Contains(lt, "e2e") {
			return prioTest, "running e2e tests"
		}
		return prioTest, "running tests"
	case lt == "install" || lt == "deps" || lt == "setup" || lt == "bootstrap" || lt == "vendor":
		return prioInstall, "installing deps"
	case lt == "lint" || lt == "vet" || strings.HasPrefix(lt, "lint"):
		return prioLint, "linting"
	case lt == "fmt" || lt == "format":
		return prioLint, "formatting"
	case lt == "clean":
		return prioFS, "cleaning the build"
	case lt == "run" || lt == "dev" || lt == "serve" || lt == "start":
		return prioRun, "running the program"
	}
	return prioBuild, p + " " + target
}

func classifyJVM(p string, pos []string) (int, string) {
	var tasks []string
	for _, a := range pos {
		tasks = append(tasks, strings.ToLower(a))
	}
	has := func(names ...string) bool {
		for _, t := range tasks {
			for _, n := range names {
				if t == n || strings.HasSuffix(t, ":"+n) {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has("test", "check", "verify", "integrationtest", "connectedcheck", "testdebugunittest"):
		return prioTest, "running tests"
	case has("build", "package", "compile", "assemble", "install", "jar", "bootjar", "assembledebug", "assemblerelease", "uberjar", "compilejava"):
		return prioBuild, "building"
	case has("dependencies", "dependency:resolve", "dependency:go-offline", "deps", "resolve"):
		return prioInstall, "installing deps"
	case has("run", "bootrun"):
		return prioRun, "running the program"
	case has("spotlessapply", "ktlintformat", "fmt", "scalafmt"):
		return prioLint, "formatting"
	}
	if len(pos) == 0 {
		return prioBuild, "building"
	}
	return prioBuild, p + " " + pos[0]
}

// runTarget names what a "run" step runs: a file, else the fallback.
func runTarget(pos []string, c Context, fallback string) string {
	for _, a := range pos {
		if a == "--" {
			continue
		}
		if a == "." {
			return "the program"
		}
		return ShortPath(a, c)
	}
	return fallback
}

func grepPattern(rest []string) string {
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "-e" || a == "--regexp" || a == "-Pattern" || a == "-pattern":
			return at(rest, i+1)
		case a == "-g" || a == "--glob" || a == "-t" || a == "--type" || a == "-A" || a == "-B" || a == "-C" ||
			a == "-m" || a == "--max-count" || a == "--include" || a == "--exclude" || a == "-f" || a == "--context" || a == "-Path":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a
		}
	}
	return ""
}

func filesOnly(pos []string) []string {
	var out []string
	for _, a := range pos {
		if strings.ContainsAny(a, "/\\.") && !strings.HasPrefix(a, "s/") {
			out = append(out, a)
		}
	}
	return out
}

func readLine(pos []string, c Context) (int, string) {
	switch len(pos) {
	case 0:
		return prioRead, "reading output"
	case 1:
		return prioRead, "reading " + ShortPath(pos[0], c)
	}
	return prioRead, "reading " + plural(len(pos), "file")
}

func many(pos []string, c Context) string {
	switch len(pos) {
	case 0:
		return "files"
	case 1:
		return ShortPath(pos[0], c)
	}
	return plural(len(pos), "file")
}
