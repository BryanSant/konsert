package digest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestCleanLines(t *testing.T) {
	raw := []byte(
		"\x1b[32mStarting build\x1b[0m\r\n" +
			"⠋\n⠙\n⠹\n/\n-\n\\\n" +
			"Downloading 10%\rDownloading 50%\rDownloading 100%\n" +
			"retrying connection...\n" +
			"retrying connection...\n" +
			"retrying connection...\n" +
			"\x1b]0;window title\x07\n" +
			"done!\n",
	)
	got := CleanLines(raw)
	want := []string{
		"Starting build",
		"Downloading 100%",
		"retrying connection... (x3)",
		"done!",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CleanLines:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractFailureTailCap30(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&b, "line %02d\n", i)
	}
	tail := ExtractFailureTail([]byte(b.String()), 0)
	if len(tail) != 30 {
		t.Fatalf("len(tail) = %d, want 30", len(tail))
	}
	if tail[0] != "line 21" || tail[29] != "line 50" {
		t.Fatalf("tail bounds = [%q .. %q], want [line 21 .. line 50]", tail[0], tail[29])
	}
}

func TestExtractQuestionContext(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 25; i++ {
		fmt.Fprintf(&b, "ctx %02d\n", i)
	}
	b.WriteString("Should I continue? [y/N]\n")
	q, ctxLines := ExtractQuestionContext([]byte(b.String()), "Should I continue?", 0)
	if q != "Should I continue?" {
		t.Fatalf("question = %q", q)
	}
	if len(ctxLines) != 20 {
		t.Fatalf("len(ctxLines) = %d, want 20", len(ctxLines))
	}
	if ctxLines[0] != "ctx 06" || ctxLines[19] != "ctx 25" {
		t.Fatalf("ctx bounds = [%q .. %q], want [ctx 06 .. ctx 25]", ctxLines[0], ctxLines[19])
	}
}

func TestClassifyTaskFailure(t *testing.T) {
	cases := []struct {
		name string
		in   FailureClassInput
		want string
	}{
		{"canceled", FailureClassInput{Canceled: true, ExitCode: 137}, protocol.ErrKindCanceled},
		{"lost", FailureClassInput{Lost: true, Canceled: true, ExitCode: 137}, protocol.ErrKindLost},
		{"timeout", FailureClassInput{TimedOut: true, ExitCode: 137}, protocol.ErrKindTimeout},
		{"oom_137", FailureClassInput{ExitCode: 137}, protocol.ErrKindOOM},
		{"oom_sigkill", FailureClassInput{ExitCode: -9}, protocol.ErrKindOOM},
		{"oom_win_job", FailureClassInput{ExitCode: int(int32(-1073741801))}, protocol.ErrKindOOM},
		{"oom_text", FailureClassInput{ExitCode: 1, Text: "FATAL ERROR: JavaScript heap out of memory"}, protocol.ErrKindOOM},
		{"disk_full", FailureClassInput{ExitCode: 1, Text: "write /tmp/a: no space left on device (ENOSPC)"}, protocol.ErrKindDiskFull},
		{"missing_tool_127", FailureClassInput{ExitCode: 127}, protocol.ErrKindMissingTool},
		{"missing_tool_sh", FailureClassInput{ExitCode: 1, Text: "bash: foo: command not found"}, protocol.ErrKindMissingTool},
		{"missing_tool_win", FailureClassInput{ExitCode: 1, Text: "'foo' is not recognized as an internal or external command"}, protocol.ErrKindMissingTool},
		{"auth", FailureClassInput{ExitCode: 1, Text: "401 Unauthorized: please run claude /login"}, protocol.ErrKindAuth},
		{"usage_limit", FailureClassInput{ExitCode: 1, Text: "You've reached your weekly limit"}, protocol.ErrKindUsageLimit},
		{"rate_limit", FailureClassInput{ExitCode: 1, Text: "HTTP 429 Too Many Requests"}, protocol.ErrKindRateLimit},
		{"generic_error", FailureClassInput{ExitCode: 1, WaitErr: errors.New("exit status 1"), Text: "compilation failed"}, protocol.ErrKindAgentError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyTaskFailure(tc.in); got != tc.want {
				t.Errorf("ClassifyTaskFailure(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseTestOutputFixtures(t *testing.T) {
	cases := []struct {
		file       string
		wantRunner string
		wantPassed int
		wantFailed int
		wantNames  []string
	}{
		{
			file:       "go_test.txt",
			wantRunner: "go",
			wantPassed: 1,
			wantFailed: 3,
			wantNames:  []string{"TestRevokeFamily", "TestRefreshHandler/expired", "TestRefreshHandler"},
		},
		{
			file:       "go_test_json.jsonl",
			wantRunner: "go",
			wantPassed: 2,
			wantFailed: 1,
			wantNames:  []string{"TestRevokeFamily"},
		},
		{
			file:       "jest.txt",
			wantRunner: "jest",
			wantPassed: 15,
			wantFailed: 2,
			wantNames:  []string{"auth fetch wrapper › retries once on 401", "auth fetch wrapper › clears session on second 401"},
		},
		{
			file:       "vitest.txt",
			wantRunner: "vitest",
			wantPassed: 12,
			wantFailed: 1,
			wantNames:  []string{"src/auth.test.ts > auth > retries once on 401"},
		},
		{
			file:       "pytest.txt",
			wantRunner: "pytest",
			wantPassed: 14,
			wantFailed: 2,
			wantNames:  []string{"tests/test_auth.py::test_rotate_token", "tests/test_auth.py::TestFamily::test_reuse_invalidates"},
		},
		{
			file:       "cargo_test.txt",
			wantRunner: "cargo",
			wantPassed: 16,
			wantFailed: 2,
			wantNames:  []string{"auth::tests::test_rotate", "auth::tests::test_reuse"},
		},
		{
			file:       "gradle.txt",
			wantRunner: "gradle",
			wantPassed: 8,
			wantFailed: 2,
			wantNames:  []string{"com.example.AuthTest > testRotate()", "com.example.AuthTest > testReuse()"},
		},
		{
			file:       "maven_surefire.txt",
			wantRunner: "maven",
			wantPassed: 10,
			wantFailed: 2,
			wantNames:  []string{"AuthTest.testRotate", "AuthTest.testReuse"},
		},
		{
			file:       "dotnet_test.txt",
			wantRunner: "dotnet",
			wantPassed: 10,
			wantFailed: 2,
			wantNames:  []string{"MyApp.Tests.AuthTests.TestRotate", "MyApp.Tests.AuthTests.TestReuse"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			raw := string(readFixture(t, tc.file))
			got, ok := ParseTestOutput(raw)
			if !ok {
				t.Fatalf("ParseTestOutput(%s) returned ok=false", tc.file)
			}
			if got.Runner != tc.wantRunner {
				t.Errorf("Runner = %q, want %q", got.Runner, tc.wantRunner)
			}
			if got.Passed != tc.wantPassed || got.Failed != tc.wantFailed {
				t.Errorf("counts = (passed=%d, failed=%d), want (passed=%d, failed=%d)",
					got.Passed, got.Failed, tc.wantPassed, tc.wantFailed)
			}
			if !reflect.DeepEqual(got.Names, tc.wantNames) {
				t.Errorf("Names:\n got: %#v\nwant: %#v", got.Names, tc.wantNames)
			}
		})
	}
}

func TestGoldenDigests(t *testing.T) {
	t.Run("pass", func(t *testing.T) {
		d := Build(Input{
			TicketSID:  "SYR-7QK3M",
			TaskStatus: protocol.TaskStatusCompleted,
			Machine:    "bocabox",
			Agent:      "claude",
			Model:      "opus",
			Duration:   412 * time.Second,
			Summary:    "Rotation with reuse detection and a single 401 retry; tests pass.",
			FinalMessage: "Implemented token rotation.\n" +
				"Interface: RefreshHandler rotates tokens; api/pkg/auth/handler.go\n",
			Git: map[string]*protocol.GitSummary{
				"api": {DiffStat: " api/pkg/auth/handler.go | 83 +++-\n 2 files changed, 74 insertions(+), 9 deletions(-)"},
				"web": {DiffStat: " web/src/api.ts | 32 ++-\n 1 file changed, 28 insertions(+), 4 deletions(-)"},
			},
		})
		got := d.Text()
		want := strings.TrimSpace(string(readFixture(t, "golden_pass.yaml")))
		if got != want {
			t.Fatalf("golden_pass mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
		}
		if tok := protocol.EstimateTokens(got); tok > protocol.DigestMaxTokens {
			t.Fatalf("pass digest tokens = %d, want <= %d", tok, protocol.DigestMaxTokens)
		}
	})

	t.Run("fail", func(t *testing.T) {
		d := Build(Input{
			TicketSID:  "SYR-7QK3M",
			TaskStatus: protocol.TaskStatusFailed,
			Machine:    "bocabox",
			Agent:      "claude",
			Model:      "opus",
			Duration:   95 * time.Second,
			ExitCode:   1,
			Summary:    "go test failed with 2 failing tests",
			Output:     readFixture(t, "go_test.txt"),
			Git: map[string]*protocol.GitSummary{
				"api": {DiffStat: " 1 file changed, 12 insertions(+), 3 deletions(-)"},
			},
		})
		got := d.Text()
		want := strings.TrimSpace(string(readFixture(t, "golden_fail.yaml")))
		if got != want {
			t.Fatalf("golden_fail mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
		}
		if tok := protocol.EstimateTokens(got); tok > protocol.DigestMaxTokens {
			t.Fatalf("fail digest tokens = %d, want <= %d", tok, protocol.DigestMaxTokens)
		}
	})

	t.Run("needs_you", func(t *testing.T) {
		out := strings.Join([]string{
			"Inspecting api/pkg/auth/handler.go",
			"Found legacy /v1/token endpoint alongside /v2/auth/refresh",
			"Acceptance criteria only mention /auth/refresh",
			"Should /v1/token also rotate refresh tokens?",
		}, "\n")
		d := Build(Input{
			TicketSID:  "SYR-7QK3M",
			TaskStatus: protocol.TaskStatusWaitingInput,
			Machine:    "bocabox",
			Agent:      "claude",
			Model:      "opus",
			Duration:   60 * time.Second,
			Summary:    "waiting for input",
			Question:   "Should /v1/token also rotate refresh tokens?",
			Output:     []byte(out),
		})
		got := d.Text()
		want := strings.TrimSpace(string(readFixture(t, "golden_needs_you.yaml")))
		if got != want {
			t.Fatalf("golden_needs_you mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
		}
		if tok := protocol.EstimateTokens(got); tok > protocol.DigestMaxTokens {
			t.Fatalf("needs_you digest tokens = %d, want <= %d", tok, protocol.DigestMaxTokens)
		}
	})
}

func TestHardTokenCapTrimOrder(t *testing.T) {
	var out strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&out, "stderr line %02d: something failed while running step %d in package auth\n", i, i)
	}
	tests := make([]string, 0, 25)
	for i := 1; i <= 25; i++ {
		tests = append(tests, fmt.Sprintf("TestVeryLongFailingTestNameNumber%02d", i))
	}
	d := protocol.RunDigest{
		TicketSID: "SYR-CAP01",
		Status:    protocol.DigestStatusFailed,
		Machine:   "bocabox",
		Agent:     "claude",
		Model:     "opus",
		DurationS: 300,
		Summary:   strings.Repeat("a very detailed failure summary ", 6),
		Failure: &protocol.DigestFailure{
			Category: protocol.ErrKindAgentError,
			Tests:    tests,
			Tail:     ExtractFailureTail([]byte(out.String()), 30),
		},
	}
	got := d.Text()
	if tok := protocol.EstimateTokens(got); tok > protocol.DigestMaxTokens {
		t.Fatalf("capped digest tokens = %d (> %d):\n%s", tok, protocol.DigestMaxTokens, got)
	}
	if !strings.Contains(got, "category: agent_error") {
		t.Fatalf("expected failure category preserved:\n%s", got)
	}
	// Ensure Text() did not mutate original slices.
	if len(d.Failure.Tail) != 30 || len(d.Failure.Tests) != 25 {
		t.Fatalf("Text() mutated receiver slices: tail=%d tests=%d", len(d.Failure.Tail), len(d.Failure.Tests))
	}
}
