package nowline

import (
	"strings"
	"testing"
)

const live = "/private/var/tmp/konsert-live/run-1/wc"

func TestSummarize(t *testing.T) {
	ctx := Context{Dir: "/var/tmp/konsert-live/run-1/wc", Home: "/Users/me"}
	cases := []struct {
		name, tool, label, want string
	}{
		// The ticket's example: PTY claude session log (Bash: …) with cd and a checkout path.
		{"claude pty bash cd test", "Bash", "Bash: cd " + live + " && python -m pytest -q", "running tests"},
		{"claude chat run", "Bash", "Run: go test ./...", "running tests"},
		{"go test scoped", "Bash", "Run: go test -race ./internal/store/...", "running tests in internal/store"},
		{"go test -run", "Bash", "Run: go test -run TestFoo ./internal/x", "running tests in internal/x"},
		{"edit relative", "Edit", "Edit: " + live + "/wc.py", "editing wc.py"},
		{"edit nested", "Edit", "Edit: /var/tmp/konsert-live/run-1/wc/src/lib/a.ts", "editing src/lib/a.ts"},
		{"write new file", "Write", "Write: " + live + "/test_wc.py", "writing test_wc.py"},
		{"read home", "Read", "Read: /Users/me/.config/konsert/config.toml", "reading ~/.config/konsert/config.toml"},
		{"read elsewhere", "Read", "Read: /etc/hosts", "reading /etc/hosts"},
		{"search", "Grep", "Search: func main", "searching for func main"},
		{"glob", "Glob", "Search: **/*.go", "searching for **/*.go"},
		{"web search", "WebSearch", "Search the web: go 1.27 release notes", "searching the web for go 1.27 release notes"},
		{"fetch", "WebFetch", "Fetch: https://www.example.com/docs/x", "fetching example.com"},
		{"subagent", "Task", "Agent: Find the flaky test", "subagent: find the flaky test"},
		{"plan", "TodoWrite", "Update plan", "updating the plan"},
		{"mcp", "mcp__konsert__set_narration", "konsert: set_narration", "set narration (konsert)"},
		// codex
		{"codex exec", "exec", "Run: bash -lc 'cd /var/tmp/konsert-live/run-1/wc && npm test'", "running tests"},
		{"codex zsh", "exec", "Run: /bin/zsh -lc \"cargo build --release\"", "building"},
		{"codex patch many", "patch", "Edit: " + live + "/a.go, " + live + "/b.go", "editing 2 files"},
		{"codex patch one", "patch", "Edit: " + live + "/a.go", "editing a.go"},
		{"codex edit files", "patch", "Edit files", "editing files"},
		{"codex pty exec", "", "exec go vet ./...", "running go vet"},
		{"codex pty edited", "", "Edited wc.py", "editing wc.py"},
		{"codex pty M", "", "M internal/x.go", "editing internal/x.go"},
		// grok / cursor / copilot (session logs and ACP)
		{"grok run", "run_terminal_command", "Run: make test", "running tests"},
		{"grok search_replace", "search_replace", "search_replace: " + live + "/wc.py", "editing wc.py"},
		{"copilot view", "view", "view", "reading a file"},
		{"copilot str_replace", "str_replace", "str_replace: /Users/me/src/app/main.go", "editing ~/src/app/main.go"},
		{"cursor shell", "Shell", "Run: pnpm install", "installing deps"},
		{"cursor StrReplace", "StrReplace", "StrReplace: " + live + "/README.md", "editing README.md"},
		{"acp backticks", "execute", "`git status`", "git status"},
		{"acp prose title", "execute", "Run the test suite", "run the test suite"},
		{"acp read kind", "read", "README.md", "reading README.md"},
		{"acp edit kind abs", "edit", live + "/main.go", "editing main.go"},
		{"acp search kind", "search", "TODO", "searching for TODO"},
		// agy / gemini
		{"agy bash", "", "Bash: npm run build", "building"},
		{"agy running", "", "Running cargo test", "running tests"},
		{"gemini readfile", "read", "ReadFile wc.py", "reading wc.py"},
		{"gemini shell", "execute", "Shell go build ./...", "building"},
		{"gemini findfiles", "search", "FindFiles '*.py'", "searching for *.py"},
		{"gemini googlesearch", "fetch", "GoogleSearch angular signals", "searching the web for angular signals"},
		// Mock engine
		{"mock read", "Read", "Read README.md", "reading README.md"},
		// Bare tool names (process/hook reports)
		{"bare bash", "Bash", "Bash", "running a command"},
		{"bare edit", "Edit", "", "editing a file"},
		{"unknown tool no label", "frobnicate", "", "using frobnicate"},
		{"unknown label", "", "Thinking about " + live + "/x.go", "Thinking about x.go"},
		// Concurrent tool calls
		{"three reads", "Read", "Read: " + live + "/a.go · Read: " + live + "/b.go · Read: /etc/c", "reading 3 files"},
		{"two edits one multi", "Edit", "Edit: a.go, b.go · Edit: c.go", "editing 3 files"},
		{"mixed", "Bash", "Read: a.go · Run: go test ./...", "running tests (+1 more)"},
		{"two searches", "Grep", "Search: foo · Search: bar", "running 2 searches"},
		// Truncated labels
		{"truncated", "Bash", "Bash: cd /var/tmp/konsert-live/run-1/wc && go test ./... 2>&1 | tail -30 && echo do...", "running tests"},
		{"truncated unicode", "Bash", "Run: npm run lint …", "linting"},
		{"empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Summarize(tc.tool, tc.label, ctx); got != tc.want {
				t.Errorf("Summarize(%q, %q) = %q, want %q", tc.tool, tc.label, got, tc.want)
			}
		})
	}
}

func TestCommand(t *testing.T) {
	ctx := Context{Dir: "/home/dev/proj", Home: "/home/dev"}
	cases := []struct{ cmd, want string }{
		// Setup steps are dropped.
		{"cd /home/dev/proj && go test ./...", "running tests"},
		{"cd /x; export CI=1; npm test", "running tests"},
		{"CI=1 GOFLAGS=-count=1 go test ./...", "running tests"},
		{"env -u FOO BAR=1 pytest tests/test_wc.py", "running tests in tests/test_wc.py"},
		{"source .venv/bin/activate && pytest -q", "running tests"},
		{"set -euo pipefail\nmake build", "building"},
		{"sudo -E apt-get install -y jq", "installing deps"},
		{"timeout 120 go test ./...", "running tests"},
		{"time cargo build", "building"},
		// The most telling step wins.
		{"npm install && npm test", "running tests"},
		{"go build ./... && go vet ./...", "building"},
		{"git add -A && git commit -m 'fix: x'", "git commit"},
		{"git add . && git commit -m x && git push", "git commit"},
		{"mkdir -p out && cp a out/", "creating out"},
		// Pipelines: the head counts.
		{"go test ./... 2>&1 | tail -20", "running tests"},
		{"cat /home/dev/proj/go.mod | head", "reading go.mod"},
		// Wrappers
		{"bash -lc 'npm ci'", "installing deps"},
		{"sh -c \"cd /home/dev/proj && make\"", "building"},
		{"pwsh -Command dotnet test", "running tests"},
		{"cmd /c msbuild app.sln", "building"},
		{"npx vitest run", "running tests"},
		{"npx --yes eslint src", "linting"},
		{"pnpm exec playwright test", "running e2e tests"},
		{"uv run pytest", "running tests"},
		{"poetry run python manage.py test", "running manage.py"},
		{"bundle exec rspec spec/models", "running tests in spec/models"},
		{"python -m pytest -x", "running tests"},
		{"python3 -m pip install -r requirements.txt", "installing deps"},
		{"python -m unittest discover", "running tests"},
		// Go
		{"go mod tidy", "tidying go modules"},
		{"go get github.com/x/y@latest", "installing deps"},
		{"go run ./cmd/konsert serve", "running cmd/konsert"},
		{"go run .", "running the program"},
		{"gofmt -l .", "formatting"},
		{"go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...", "running honnef.co/go/tools/cmd/staticcheck@v0.8.2"},
		{"staticcheck ./...", "linting (staticcheck)"},
		{"golangci-lint run", "linting (golangci-lint)"},
		// JS/TS
		{"npm run build", "building"},
		{"npm run test:unit", "running tests"},
		{"npm run e2e", "running e2e tests"},
		{"npm run test:e2e", "running e2e tests"},
		{"npm ci", "installing deps"},
		{"yarn", "installing deps"},
		{"yarn add -D vitest", "installing deps"},
		{"pnpm lint", "linting"},
		{"pnpm dev", "starting the dev server"},
		{"bun test", "running tests"},
		{"bun run index.ts", "running index.ts"},
		{"npm run deploy", "npm run deploy"},
		{"node scripts/gen.js", "running scripts/gen.js"},
		{"npx tsc --noEmit", "type-checking"},
		{"tsc -p .", "building"},
		{"ng test --watch=false", "running tests"},
		{"ng build", "building"},
		{"vite build", "building"},
		{"jest src/a.test.ts", "running tests in src/a.test.ts"},
		{"prettier --write .", "formatting"},
		{"deno test -A", "running tests"},
		// Python
		{"pytest", "running tests"},
		{"tox -e py312", "running tests"},
		{"pip install -e .", "installing deps"},
		{"uv sync", "installing deps"},
		{"uv pip install ruff", "installing deps"},
		{"poetry install", "installing deps"},
		{"ruff check .", "linting"},
		{"ruff format .", "formatting"},
		{"black .", "formatting"},
		{"mypy src", "type-checking"},
		{"python wc.py README.md", "running wc.py"},
		{"python3 -c 'print(1)'", "running python"},
		// Rust / C / build systems
		{"cargo test --all", "running tests"},
		{"cargo nextest run", "running tests"},
		{"cargo build --release", "building"},
		{"cargo clippy -- -D warnings", "linting (clippy)"},
		{"cargo fmt --check", "formatting"},
		{"cargo add serde", "installing deps"},
		{"cargo run -- --help", "running the program"},
		{"make", "building"},
		{"make test", "running tests"},
		{"make -j8 check", "running tests"},
		{"make lint", "linting"},
		{"make install", "installing deps"},
		{"make clean", "cleaning the build"},
		{"make docs", "make docs"},
		{"just test", "running tests"},
		{"cmake -S . -B build", "configuring the build"},
		{"cmake --build build -j", "building"},
		{"ctest --output-on-failure", "running tests"},
		{"gcc -O2 -o wc wc.c", "building"},
		{"zig build test", "running tests"},
		{"bazel test //...", "running tests"},
		// JVM / .NET / Apple / others
		{"./gradlew test", "running tests"},
		{"./gradlew assembleDebug", "building"},
		{"mvn -q package", "building"},
		{"mvn test", "running tests"},
		{"sbt compile", "building"},
		{"dotnet test", "running tests"},
		{"dotnet build -c Release", "building"},
		{"dotnet restore", "installing deps"},
		{"swift test", "running tests"},
		{"swift build", "building"},
		{"xcodebuild -scheme App test", "running tests"},
		{"mix test", "running tests"},
		{"mix deps.get", "installing deps"},
		{"rspec", "running tests"},
		{"bundle install", "installing deps"},
		{"composer install", "installing deps"},
		{"phpunit tests/Unit", "running tests in tests/Unit"},
		{"flutter test", "running tests"},
		{"flutter pub get", "flutter pub"},
		{"brew install ripgrep", "installing deps"},
		{"pod install", "installing deps"},
		{"docker build -t app .", "building an image"},
		{"docker compose up -d", "starting containers"},
		{"docker-compose down", "stopping containers"},
		{"docker run --rm app", "running a container"},
		// git and GitHub
		{"git status", "git status"},
		{"git -C /home/dev/proj diff --stat", "git diff"},
		{"git log --oneline -5", "git log"},
		{"git push -u origin HEAD", "git push"},
		{"git rebase -i main", "git rebase"},
		{"git grep -n TODO", "searching for TODO"},
		{"gh pr create --fill", "gh pr create"},
		{"gh pr view 12", "gh pr view"},
		{"gh run watch", "gh run watch"},
		// Search, read, list
		{"grep -rn 'func main' .", "searching for func main"},
		{"rg -n --glob '*.go' handleReview internal", "searching for handleReview"},
		{"rg -e 'a|b' src", "searching for a|b"},
		{"find . -name '*.py' -type f", "finding *.py"},
		{"find /home/dev/proj/internal", "finding files in internal"},
		{"fd test_", "finding test_"},
		{"cat README.md", "reading README.md"},
		{"head -50 /home/dev/proj/a.go /home/dev/proj/b.go", "reading 2 files"},
		{"tail -f /home/dev/logs/x.log", "reading ~/logs/x.log"},
		{"sed -n '1,80p' internal/x.go", "reading internal/x.go"},
		{"sed -i 's/a/b/' internal/x.go", "editing internal/x.go"},
		{"wc -l wc.py", "counting lines in wc.py"},
		{"ls -la", "listing files"},
		{"ls /home/dev/proj/web", "listing web"},
		{"tree -L 2", "listing files"},
		{"diff a.txt b.txt", "comparing files"},
		{"jq .version package.json", "reading package.json"},
		// Files, network, processes
		{"rm -rf /home/dev/proj/build", "removing build"},
		{"rm a b c", "removing 3 files"},
		{"mv old.go new.go", "moving old.go"},
		{"cp -r src /home/dev/backup", "copying src"},
		{"chmod +x run.sh", "changing permissions"},
		{"curl -fsSL https://api.github.com/repos/x/y", "fetching api.github.com"},
		{"wget -q example.org/file.tgz", "fetching example.org/file.tgz"},
		{"sleep 5", "waiting"},
		{"pkill -f konsertd", "stopping processes"},
		{"echo done", "printing"},
		{"./scripts/release.sh v1", "running scripts/release.sh"},
		{"/home/dev/proj/bin/konsert serve", "running bin/konsert"},
		{"/usr/local/bin/foo", "running /usr/local/bin/foo"},
		{"jqx --flag", "running jqx"},
		// Only setup
		{"cd /home/dev/proj", "running cd"},
		{"", "running a command"},
		// Windows
		{`cd C:\work\proj && go test ./...`, "running tests"},
		{"Get-Content README.md", "reading README.md"},
		{"Select-String -Pattern TODO -Path *.cs", "searching for TODO"},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			if got := Command(tc.cmd, ctx).text; got != tc.want {
				t.Errorf("Command(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}

func TestShortPath(t *testing.T) {
	cases := []struct {
		name string
		p    string
		c    Context
		want string
	}{
		{"under dir", "/repo/src/a.go", Context{Dir: "/repo"}, "src/a.go"},
		{"dir itself", "/repo", Context{Dir: "/repo/"}, "."},
		{"sibling with prefix", "/repository/a.go", Context{Dir: "/repo"}, "/repository/a.go"},
		{"private alias", "/private/var/tmp/x/a.go", Context{Dir: "/var/tmp/x"}, "a.go"},
		{"private alias reversed", "/var/tmp/x/a.go", Context{Dir: "/private/var/tmp/x"}, "a.go"},
		{"home", "/Users/me/notes.md", Context{Dir: "/repo", Home: "/Users/me"}, "~/notes.md"},
		{"home itself", "/Users/me", Context{Home: "/Users/me"}, "~"},
		{"home guessed from dir", "/home/dev/.bashrc", Context{Dir: "/home/dev/src/x"}, "~/.bashrc"},
		{"home guessed from path", "/Users/other/x", Context{}, "~/x"},
		{"windows dir", `C:\work\proj\src\main.rs`, Context{Dir: `C:\work\proj`}, "src/main.rs"},
		{"windows home", `C:\Users\me\AppData\x`, Context{}, "~/AppData/x"},
		{"relative stays", "src/a.go", Context{Dir: "/repo"}, "src/a.go"},
		{"quoted", `"/repo/a b.go"`, Context{Dir: "/repo"}, "a b.go"},
		{"empty", "", Context{Dir: "/repo"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.c
			if c.Home == "" {
				c.Home = guessHome(c.Dir)
			}
			if got := ShortPath(tc.p, c); got != tc.want {
				t.Errorf("ShortPath(%q, %+v) = %q, want %q", tc.p, tc.c, got, tc.want)
			}
		})
	}
}

func TestSummarizeCapsLength(t *testing.T) {
	// A search pattern is capped at 40 runes ("searching for " + 39 + "…").
	got := Summarize("Grep", "Search: "+strings.Repeat("a", 95), Context{})
	if r := []rune(got); len(r) != len("searching for ")+40 || r[len(r)-1] != '…' {
		t.Errorf("Summarize(long) = %q", got)
	}
	// Any line is capped at maxLen.
	unknown := "Doing " + strings.Repeat("b", 120)
	if got := Summarize("", unknown, Context{}); len([]rune(got)) != maxLen {
		t.Errorf("Summarize(unknown long) has %d runes, want %d", len([]rune(got)), maxLen)
	}
}
