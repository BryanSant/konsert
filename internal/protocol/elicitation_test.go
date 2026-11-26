package protocol

import (
	"strings"
	"testing"
)

func TestFormatElicitationTextGolden(t *testing.T) {
	cases := []struct {
		name string
		el   *AgentElicitation
		want string
	}{
		{
			name: "agy_commandline_raw_json",
			el: &AgentElicitation{
				ID:     "perm-1",
				Kind:   ElicitPermission,
				Title:  "cd /work/.syre/tickets/tk-ka8r5/demo && ls -la && git status -sb",
				Detail: `{"CommandLine":"cd /work/.syre/tickets/tk-ka8r5/demo && ls -la && git status -sb","WaitMsBeforeAsync":5000,"Cwd":"/work/.syre/tickets/tk-ka8r5"}`,
				ToolCall: &AgentToolCall{
					ID:    "tc-1",
					Name:  "run_command",
					Kind:  "execute",
					Title: "cd /work/.syre/tickets/tk-ka8r5/demo && ls -la && git status -sb",
					Input: `{"CommandLine":"cd /work/.syre/tickets/tk-ka8r5/demo && ls -la && git status -sb","WaitMsBeforeAsync":5000}`,
				},
			},
			want: "permission: run cd /work/.syre/tickets/tk-ka8r5/demo && ls -la && git status -sb",
		},
		{
			name: "claude_bash_command",
			el: &AgentElicitation{
				ID:    "perm-2",
				Kind:  ElicitPermission,
				Title: "Allow Run: ls -la?",
				ToolCall: &AgentToolCall{
					Name:  "Bash",
					Kind:  "execute",
					Input: `{"command":"ls -la"}`,
				},
			},
			want: "permission: run ls -la",
		},
		{
			name: "cmd_field_shape",
			el: &AgentElicitation{
				ID:     "perm-3",
				Kind:   ElicitPermission,
				Title:  "Run command",
				Detail: `{"cmd":"go test ./..."}`,
			},
			want: "permission: run go test ./...",
		},
		{
			name: "edit_file_path",
			el: &AgentElicitation{
				ID:    "perm-4",
				Kind:  ElicitPermission,
				Title: "Allow Edit: internal/controller/pulse.go?",
				ToolCall: &AgentToolCall{
					Name:  "Edit",
					Kind:  "edit",
					Input: `{"file_path":"internal/controller/pulse.go","old_string":"a","new_string":"b"}`,
				},
			},
			want: "permission: edit internal/controller/pulse.go",
		},
		{
			name: "edit_path_field",
			el: &AgentElicitation{
				ID:     "perm-5",
				Kind:   ElicitPermission,
				Title:  "Write file",
				Detail: `{"path":"README.md","content":"hello"}`,
			},
			want: "permission: edit README.md",
		},
		{
			name: "read_file_path",
			el: &AgentElicitation{
				ID:    "perm-6",
				Kind:  ElicitPermission,
				Title: "Read file",
				ToolCall: &AgentToolCall{
					Name:  "Read",
					Kind:  "read",
					Input: `{"file_path":"/etc/hosts"}`,
				},
			},
			want: "permission: read /etc/hosts",
		},
		{
			name: "long_command_capped_120",
			el: &AgentElicitation{
				ID:   "perm-7",
				Kind: ElicitPermission,
				ToolCall: &AgentToolCall{
					Name:  "Bash",
					Kind:  "execute",
					Input: `{"command":"go test -v -race -count=1 ./internal/controller/... ./internal/worker/... ./internal/protocol/... ./internal/store/... ./cmd/..."}`,
				},
			},
			want: "permission: run go test -v -race -count=1 ./internal/controller/... ./internal/worker/... ./internal/protocol/... ./i...",
		},
		{
			name: "question_as_text",
			el: &AgentElicitation{
				ID:    "q-1",
				Kind:  ElicitQuestion,
				Title: "Which database migration strategy should we use for legacy rows?",
			},
			want: "Which database migration strategy should we use for legacy rows?",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatElicitationText(tc.el)
			if got != tc.want {
				t.Fatalf("FormatElicitationText:\n got: %q (len=%d)\nwant: %q (len=%d)", got, len([]rune(got)), tc.want, len([]rune(tc.want)))
			}
			if len([]rune(got)) > MaxElicitationTextChars {
				t.Fatalf("exceeded %d chars: %d (%q)", MaxElicitationTextChars, len([]rune(got)), got)
			}
			if strings.Contains(got, `{"`) || strings.Contains(got, "WaitMsBeforeAsync") {
				t.Fatalf("raw JSON leaked into formatted elicitation: %q", got)
			}
		})
	}
}

func TestRunDigestNeedsYouPermissionGolden(t *testing.T) {
	el := &AgentElicitation{
		ID:     "perm-1",
		Kind:   ElicitPermission,
		Title:  "ls -la",
		Detail: `{"CommandLine":"ls -la","WaitMsBeforeAsync":5000}`,
		ToolCall: &AgentToolCall{
			Name:  "run_command",
			Kind:  "execute",
			Input: `{"CommandLine":"ls -la","WaitMsBeforeAsync":5000}`,
		},
	}
	d := RunDigest{
		TicketSID: "tk-ka8r5",
		Attempt:   1,
		Status:    DigestStatusNeedsYou,
		Machine:   "mini-mac",
		Agent:     "agy",
		Question:  &DigestQuestion{Text: FormatElicitationText(el)},
	}
	got := d.Text()
	want := strings.Join([]string{
		"ticket: tk-ka8r5",
		"attempt: 1",
		"status: needs_you",
		"machine: mini-mac (agy)",
		"question:",
		`  text: "permission: run ls -la"`,
	}, "\n")
	if got != want {
		t.Fatalf("digest mismatch:\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}
