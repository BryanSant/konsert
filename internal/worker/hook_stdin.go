package worker

import (
	"encoding/json/v2"
	"io"
	"os"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// maxHookStdin caps what `syred hook-event` reads from an agent hook's stdin.
const maxHookStdin = 256 << 10

// hookStdinWait bounds waiting for a hook's stdin (agents write it at once and close it).
const hookStdinWait = 500 * time.Millisecond

// hookInput is the JSON Claude Code (and compatible agents) pass to hook commands on stdin.
type hookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Event     string `json:"hook_event_name"`
	ToolName  string `json:"tool_name"`
	Message   string `json:"message"`
	ToolInput struct {
		Command     string `json:"command"`
		FilePath    string `json:"file_path"`
		Description string `json:"description"`
		Pattern     string `json:"pattern"`
	} `json:"tool_input"`
}

// ReadHookStdin fills ev from the hook JSON on stdin, when stdin is a pipe (not a terminal)
// and delivers it promptly. Values given on the command line win.
func ReadHookStdin(ev *HookEvent) {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice != 0 {
		return
	}
	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(os.Stdin, maxHookStdin))
		done <- b
	}()
	select {
	case b := <-done:
		ApplyHookInput(ev, b)
	case <-time.After(hookStdinWait):
	}
}

// ApplyHookInput merges a hook's stdin JSON into ev (fields already set are kept).
func ApplyHookInput(ev *HookEvent, raw []byte) {
	var in hookInput
	if len(raw) == 0 || json.Unmarshal(raw, &in, protocol.JSONOptions) != nil {
		return
	}
	if ev.SessionID == "" {
		ev.SessionID = in.SessionID
	}
	if ev.Cwd == "" {
		ev.Cwd = in.Cwd
	}
	if in.Event != "" && (ev.Event == "" || ev.Event == "working") {
		ev.Event = in.Event
	}
	if ev.Tool == "" {
		ev.Tool = in.ToolName
	}
	if ev.Message == "" {
		switch {
		case in.Message != "":
			ev.Message = in.Message
		case in.ToolName != "":
			ev.Message = hookToolLabel(in)
		}
	}
	if ev.FilePath == "" && in.ToolInput.FilePath != "" && isEditTool(in.ToolName) {
		ev.FilePath = in.ToolInput.FilePath
	}
}

func isEditTool(name string) bool {
	switch strings.ToLower(name) {
	case "edit", "write", "multiedit", "notebookedit":
		return true
	}
	return false
}

// hookToolLabel is a short line for a tool call ("Bash: go test ./...").
func hookToolLabel(in hookInput) string {
	arg := in.ToolInput.Description
	switch {
	case in.ToolInput.Command != "":
		arg = in.ToolInput.Command
	case in.ToolInput.FilePath != "":
		arg = in.ToolInput.FilePath
	case in.ToolInput.Pattern != "":
		arg = in.ToolInput.Pattern
	}
	arg = strings.Join(strings.Fields(arg), " ")
	if r := []rune(arg); len(r) > 100 {
		arg = string(r[:99]) + "…"
	}
	if arg == "" {
		return in.ToolName
	}
	return in.ToolName + ": " + arg
}
