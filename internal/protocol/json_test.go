package protocol

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"
)

// Wire-format guarantees of the encoding/json/v2 migration: zero numbers/bools
// tagged omitzero stay absent (v1 omitempty semantics), nil slices and maps
// encode as [] and {} (v1: null), and invalid UTF-8 is replaced, not fatal.

func envelopePayload(t *testing.T, payload any) string {
	t.Helper()
	b, err := NewEnvelope("x", "", payload)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Payload jsontext.Value `json:"payload"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	return string(env.Payload)
}

func TestWireOmitZero(t *testing.T) {
	cases := []struct {
		name    string
		payload any
		want    string
	}{
		{"zero usage", AgentTokenUsage{}, `{}`},
		{"usage", AgentTokenUsage{OutputTokens: 3}, `{"output_tokens":3}`},
		{"rate limit", AgentRateLimit{Window: "five_hour"}, `{"window":"five_hour"}`},
		{"error", AgentChatError{Kind: ErrKindExit, Message: "m"}, `{"kind":"exit","message":"m"}`},
		{"chat event", AgentChatEvent{Kind: EventTurnEnd}, `{"chat_id":"","seq":0,"kind":"turn_end","at":"0001-01-01T00:00:00Z"}`},
		{"metrics", WorkerMetrics{CPUPercent: 1}, `{"cpu_percent":1,"mem_used_mb":0,"mem_total_mb":0,"load_avg":0}`},
		{"task start", TaskStartPayload{TaskID: "t"}, `{"task_id":"t","agent":"","workdir":"","use_worktree":false,"prompt":"","mode":"","permission_mode":"","timeout_seconds":0}`},
		// An `any` field holding an empty value is still sent (only nil is omitted).
		{"fleet accounts", FleetUpdatePayload{Kind: "agent_accounts", Accounts: []AgentAccount{}}, `{"kind":"agent_accounts","accounts":[]}`},
		{"chat tool", ChatToolPayload{SessionID: "s", Result: ""}, `{"session_id":"s","call_id":"","name":"","result":"","status":""}`},
	}
	for _, tc := range cases {
		if got := envelopePayload(t, tc.payload); got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, got, tc.want)
		}
	}
}

func TestWireNilCollections(t *testing.T) {
	cases := []struct {
		name    string
		payload any
		want    string
	}{
		{"fs list", FSListResult{CurrentDir: "/"}, `{"current_dir":"/","entries":[]}`},
		{"git inspect", GitInspectResult{}, `{"is_git_repo":false,"is_worktree":false,"added":0,"deleted":0,"files":[]}`},
		{"heartbeat", WorkerHeartbeatPayload{}, `{"active_task_ids":[]}`},
		{"worker info agents/tags", WorkerInfo{ID: "w"}, `{"id":"w","name":"","host_group":"","os":"","arch":"","isolation":"","permission_mode":"","agents":[],"tags":{},"max_concurrent":0}`},
		{"accounts report", AgentCapabilitiesReport{}, `{"accounts":[]}`},
	}
	for _, tc := range cases {
		if got := envelopePayload(t, tc.payload); got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, got, tc.want)
		}
	}
}

func TestEnvelopeShape(t *testing.T) {
	b, err := NewEnvelope("pty.subscribe", "id1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "payload") {
		t.Fatalf("nil payload must be omitted: %s", b)
	}
	env, err := DecodeEnvelope(b)
	if err != nil || env.Type != "pty.subscribe" || env.ID != "id1" || time.Since(env.TS) > time.Minute {
		t.Fatalf("envelope = %+v, %v", env, err)
	}
	b, err = NewEnvelope(MsgTaskStatus, "", TaskStatusPayload{TaskID: "t", Status: TaskStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	env, _ = DecodeEnvelope(b)
	p, err := DecodePayload[TaskStatusPayload](env)
	if err != nil || p.TaskID != "t" || p.Status != TaskStatusRunning {
		t.Fatalf("payload = %+v, %v", p, err)
	}
}

// File names, command and agent output can carry invalid UTF-8: the frame is
// still sent with U+FFFD in place of the bad bytes, and decodes.
func TestEnvelopeInvalidUTF8(t *testing.T) {
	b, err := NewEnvelope(MsgFSListResult, "", FSListResult{CurrentDir: "/d", Entries: []FSEntry{{Name: "bad\xffname", Path: "/d/bad\xffname"}}})
	if err != nil {
		t.Fatal(err)
	}
	env, err := DecodeEnvelope(b)
	if err != nil {
		t.Fatal(err)
	}
	res, err := DecodePayload[FSListResult](env)
	if err != nil || len(res.Entries) != 1 || res.Entries[0].Name != "bad\uFFFDname" {
		t.Fatalf("decoded = %+v, %v", res, err)
	}
	// Incoming frames with invalid UTF-8 (or lone surrogate escapes) decode too.
	env, err = DecodeEnvelope([]byte("{\"type\":\"chat.send\",\"ts\":\"2026-01-01T00:00:00Z\",\"payload\":{\"session_id\":\"s\",\"content\":\"a\xffb \\ud800\"}}"))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := DecodePayload[ChatSendPayload](env)
	if err != nil || cs.Content != "a\uFFFDb \uFFFD" {
		t.Fatalf("chat.send = %+v, %v", cs, err)
	}
}

// Names are matched exactly: a differently cased member is ignored (v1 matched it).
func TestDecodeCaseSensitive(t *testing.T) {
	env, err := DecodeEnvelope([]byte(`{"type":"task.cancel","ts":"2026-01-01T00:00:00Z","payload":{"Task_ID":"t"}}`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := DecodePayload[TaskCancelPayload](env)
	if err != nil || p.TaskID != "" {
		t.Fatalf("payload = %+v, %v", p, err)
	}
}
