package worker

import (
	"testing"
)

func TestApplyHookInputAndParse(t *testing.T) {
	ev := HookEvent{Agent: "claude", Event: "PreToolUse"}
	ApplyHookInput(&ev, []byte(`{"session_id":"s1","cwd":"/repo","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"go test ./..."}}`))
	if ev.SessionID != "s1" || ev.Cwd != "/repo" || ev.Tool != "Bash" || ev.Message != "Bash: go test ./..." {
		t.Fatalf("event %+v", ev)
	}
	line := `{"session_id":"s1","agent":"claude","event":"PreToolUse","cwd":"/repo","message":"Bash: go test ./...","tool":"Bash"}`
	p, ok := ParseHookEventLine(line)
	if !ok || p.Tool != "Bash" || p.Event != "PreToolUse" || p.Status != "working" {
		t.Fatalf("payload %+v", p)
	}
	// PostToolUse: the tool is over.
	p, _ = ParseHookEventLine(`{"session_id":"s1","agent":"claude","event":"PostToolUse","tool":"Bash"}`)
	if p.Tool != "" || p.Event != "PostToolUse" {
		t.Fatalf("post payload %+v", p)
	}
	// Command-line values win; garbage is ignored.
	ev2 := HookEvent{SessionID: "flag", Event: "Notification"}
	ApplyHookInput(&ev2, []byte(`{"session_id":"stdin","message":"Claude needs your permission","hook_event_name":"Notification"}`))
	if ev2.SessionID != "flag" || ev2.Message != "Claude needs your permission" {
		t.Fatalf("event %+v", ev2)
	}
	ApplyHookInput(&ev2, []byte(`not json`))
	// An edit hook records the file.
	ev3 := HookEvent{Event: "PostToolUse"}
	ApplyHookInput(&ev3, []byte(`{"tool_name":"Edit","tool_input":{"file_path":"/repo/main.go"}}`))
	if ev3.FilePath != "/repo/main.go" {
		t.Fatalf("edit file %+v", ev3)
	}
}

func TestParseGPUOutputs(t *testing.T) {
	io := `+-o AGXAcceleratorG14X  <class AGXAcceleratorG14X>
    "PerformanceStatistics" = {"Device Utilization %"=38,"Renderer Utilization %"=30}
  +-o Other <class X>
    "PerformanceStatistics" = {"Device Utilization %"=52}`
	if v := parseIORegGPU(io); v == nil || *v != 52 {
		t.Fatalf("ioreg: %v", v)
	}
	if v := parseIORegGPU("no gpu"); v != nil {
		t.Fatalf("ioreg without stats: %v", *v)
	}
	if v := parseNvidiaSMIGPU("12\n87\n"); v == nil || *v != 87 {
		t.Fatalf("nvidia-smi: %v", v)
	}
	if v := parseNvidiaSMIGPU("[N/A]\n"); v != nil {
		t.Fatalf("nvidia-smi N/A: %v", *v)
	}
}
