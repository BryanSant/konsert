package worker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/chat"
	"github.com/BryanSant/konsert/internal/worker/drivers"
)

// capturingDriver records LaunchOptions and then fails to launch.
type capturingDriver struct {
	missingBinaryDriver
	mu   sync.Mutex
	opts []drivers.LaunchOptions
}

func (d *capturingDriver) Name() string { return "claude" }
func (d *capturingDriver) BuildCommand(o drivers.LaunchOptions) drivers.LaunchSpec {
	d.mu.Lock()
	d.opts = append(d.opts, o)
	d.mu.Unlock()
	return d.missingBinaryDriver.BuildCommand(o)
}

type fakeAccounts map[string]chat.Account

func (f fakeAccounts) Resolve(agent, id string) (chat.Account, error) {
	a, ok := f[id]
	if !ok || a.Agent != agent {
		return chat.Account{}, errors.New("no such account")
	}
	return a, nil
}

func TestRunnerAppliesAccountApprovalAndEffort(t *testing.T) {
	drv := &capturingDriver{}
	reg := drivers.NewRegistry()
	reg.Register(drv)
	rec := newStatusRecorder()
	r := NewRunner(reg, false, nil, RunnerCallbacks{OnTaskStatus: rec.record}, nil)
	r.SetAccounts(fakeAccounts{"claude2": {ID: "claude2", Agent: "claude", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/c2", "CLAUDE_CODE_OAUTH_TOKEN": "tok"}}})

	_ = r.StartTask(context.Background(), protocol.TaskStartPayload{
		TaskID: "t-acct", Agent: "claude", Workdir: t.TempDir(), AccountID: "claude2", ApprovalMode: "acceptEdits", Effort: "high",
	})
	drv.mu.Lock()
	got := drv.opts
	drv.mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("BuildCommand calls = %d", len(got))
	}
	o := got[0]
	if o.AccountID != "claude2" || o.ApprovalMode != "acceptEdits" || o.Effort != "high" {
		t.Fatalf("launch options = %+v", o)
	}
	if !slices.Contains(o.Env, "CLAUDE_CONFIG_DIR=/c2") || !slices.Contains(o.Env, "CLAUDE_CODE_OAUTH_TOKEN=tok") {
		t.Fatalf("account env not applied: %v", o.Env)
	}

	// Unknown account: the task fails before launch.
	_ = r.StartTask(context.Background(), protocol.TaskStartPayload{TaskID: "t-bad", Agent: "claude", Workdir: t.TempDir(), AccountID: "nope"})
	var failed bool
	for _, s := range rec.snapshot() {
		if s.TaskID == "t-bad" && s.Status == protocol.TaskStatusFailed && strings.Contains(s.Summary, "nope") {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("statuses = %+v", rec.snapshot())
	}
}

func TestSandboxedRunnerRefusesFullAccessApprovalMode(t *testing.T) {
	drv := &capturingDriver{}
	reg := drivers.NewRegistry()
	reg.Register(drv)
	rec := newStatusRecorder()
	r := NewRunner(reg, true, nil, RunnerCallbacks{OnTaskStatus: rec.record}, nil)
	err := r.StartTask(context.Background(), protocol.TaskStartPayload{
		TaskID: "t-yolo", Agent: "claude", Workdir: t.TempDir(), PermissionMode: protocol.PermissionSandboxed, ApprovalMode: "bypassPermissions",
	})
	if err == nil || len(drv.opts) != 0 {
		t.Fatalf("sandboxed worker launched a full-access approval mode (err=%v)", err)
	}
}
