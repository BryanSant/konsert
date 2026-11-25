package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/store/sqlite"
)

func TestWorkerDefaultShellAndTerminalDispatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(config.ControllerConfig{NoAuth: true}, st, nil, "test")
	th := &testHub{h: srv.hub, st: st, svc: srv.service, ts: httptest.NewServer(srv)}
	t.Cleanup(th.stop)

	info := testWorkerInfo("w-win", "winbox")
	info.OS = "windows"
	info.Agents = []string{"shell"}
	info.Facts = &protocol.MachineFacts{
		Shells: []protocol.TerminalShell{
			{ID: protocol.ShellGitBash, Label: "Git Bash", Path: `C:\Program Files\Git\bin\bash.exe`},
			{ID: protocol.ShellPwsh, Label: "PowerShell 7", Path: `C:\Program Files\PowerShell\7\pwsh.exe`},
		},
	}
	fw := th.connectWorker(t, info, nil, "w-win")

	// Set per-machine default shell via REST API.
	req, _ := http.NewRequest(http.MethodPut, th.ts.URL+"/api/workers/w-win/default-shell", strings.NewReader(`{"default_shell":"git-bash"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Syre-Request", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT default-shell: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT default-shell status = %d, want 200", resp.StatusCode)
	}

	// Reject unknown shell ID.
	badReq, _ := http.NewRequest(http.MethodPut, th.ts.URL+"/api/workers/w-win/default-shell", strings.NewReader(`{"default_shell":"tcsh"}`))
	badReq.Header.Set("Content-Type", "application/json")
	badReq.Header.Set("X-Syre-Request", "1")
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatalf("PUT unknown default-shell: %v", err)
	}
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT unknown default-shell status = %d, want 400", badResp.StatusCode)
	}

	// DefaultShell survives a subsequent UpsertWorker (worker reconnect).
	if err := th.st.UpsertWorker(ctx, store.Worker{
		ID: "w-win", Name: "winbox", HostGroup: "winbox", OS: "windows", Arch: "amd64",
		Agents: []string{"shell"}, Status: "online", LastSeenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	wk, err := th.st.GetWorker(ctx, "w-win")
	if err != nil || wk.DefaultShell != protocol.ShellGitBash {
		t.Fatalf("stored DefaultShell = %q, err = %v; want git-bash", wk.DefaultShell, err)
	}

	// Terminal dispatch without explicit shell inherits machine default_shell.
	_, err = th.svc.DispatchTask(ctx, DispatchRequest{
		WorkerSelector: WorkerSelector{WorkerID: "w-win"},
		SessionKind:    "terminal",
		Agent:          "shell",
		RepoPath:       "~",
		Mode:           protocol.TaskModeInteractive,
	})
	if err != nil {
		t.Fatalf("dispatch default terminal: %v", err)
	}
	env := fw.expect(protocol.MsgTaskStart, 3*time.Second)
	start, err := protocol.DecodePayload[protocol.TaskStartPayload](env)
	if err != nil || start.Shell != protocol.ShellGitBash {
		t.Fatalf("dispatched shell = %q (err=%v), want git-bash", start.Shell, err)
	}

	// Explicit per-terminal shell overrides machine default.
	_, err = th.svc.DispatchTask(ctx, DispatchRequest{
		WorkerSelector: WorkerSelector{WorkerID: "w-win"},
		SessionKind:    "terminal",
		Agent:          "shell",
		Shell:          protocol.ShellPwsh,
		RepoPath:       "~",
		Mode:           protocol.TaskModeInteractive,
	})
	if err != nil {
		t.Fatalf("dispatch explicit terminal: %v", err)
	}
	env = fw.expect(protocol.MsgTaskStart, 3*time.Second)
	start, err = protocol.DecodePayload[protocol.TaskStartPayload](env)
	if err != nil || start.Shell != protocol.ShellPwsh {
		t.Fatalf("override shell = %q (err=%v), want pwsh", start.Shell, err)
	}

	// FleetStatus exposes both facts.shells and default_shell.
	fs, err := th.svc.FleetStatus(ctx)
	if err != nil || len(fs.Workers) != 1 {
		t.Fatalf("FleetStatus = %+v, %v", fs, err)
	}
	b, _ := json.Marshal(fs.Workers[0], protocol.JSONOptions)
	if !strings.Contains(string(b), `"default_shell":"git-bash"`) || !strings.Contains(string(b), `"id":"git-bash"`) {
		t.Fatalf("worker view JSON missing shell fields: %s", b)
	}
}

func TestWorkerAgentsUseGitBashAndAgentDispatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(config.ControllerConfig{NoAuth: true}, st, nil, "test")
	th := &testHub{h: srv.hub, st: st, svc: srv.service, ts: httptest.NewServer(srv)}
	t.Cleanup(th.stop)

	info := testWorkerInfo("w-win-gb", "winbox")
	info.OS = "windows"
	info.MaxConcurrent = 4
	info.Agents = []string{"claude", "shell"}
	info.Facts = &protocol.MachineFacts{
		Shells: []protocol.TerminalShell{
			{ID: protocol.ShellGitBash, Label: "Git Bash", Path: `C:\Program Files\Git\bin\bash.exe`},
			{ID: protocol.ShellPwsh, Label: "PowerShell 7", Path: `C:\Program Files\PowerShell\7\pwsh.exe`},
		},
	}
	fw := th.connectWorker(t, info, nil, "w-win-gb")

	// Default is on (true).
	wk, err := th.st.GetWorker(ctx, "w-win-gb")
	if err != nil || !wk.AgentsUseGitBash {
		t.Fatalf("default AgentsUseGitBash = %v (err=%v), want true", wk.AgentsUseGitBash, err)
	}

	// Chat open payload inherits UseGitBash=true from the worker row.
	chatSess := store.ChatSession{
		ID: "chat-gb-1", SID: "c-gb1", WorkerID: "w-win-gb", Agent: "claude",
		Cwd: `C:\repo`, Title: "test", Status: "active", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := th.st.CreateChatSession(ctx, chatSess); err != nil {
		t.Fatalf("CreateChatSession: %v", err)
	}
	openOn, err := th.h.buildChatOpen(ctx, chatSess)
	if err != nil || !openOn.UseGitBash {
		t.Fatalf("buildChatOpen default UseGitBash = %v (err=%v), want true", openOn.UseGitBash, err)
	}

	// Agent session receives UseGitBash=true by default.
	_, err = th.svc.DispatchTask(ctx, DispatchRequest{
		WorkerSelector: WorkerSelector{WorkerID: "w-win-gb"},
		SessionKind:    "chat",
		Agent:          "claude",
		Prompt:         "hello",
		RepoPath:       "~",
		Mode:           protocol.TaskModeInteractive,
	})
	if err != nil {
		t.Fatalf("dispatch claude: %v", err)
	}
	env := fw.expect(protocol.MsgTaskStart, 3*time.Second)
	start, err := protocol.DecodePayload[protocol.TaskStartPayload](env)
	if err != nil || !start.UseGitBash {
		t.Fatalf("dispatched claude UseGitBash = %v (err=%v), want true", start.UseGitBash, err)
	}

	// Terminal session does not set UseGitBash.
	_, err = th.svc.DispatchTask(ctx, DispatchRequest{
		WorkerSelector: WorkerSelector{WorkerID: "w-win-gb"},
		SessionKind:    "terminal",
		Agent:          "shell",
		RepoPath:       "~",
		Mode:           protocol.TaskModeInteractive,
	})
	if err != nil {
		t.Fatalf("dispatch terminal: %v", err)
	}
	env = fw.expect(protocol.MsgTaskStart, 3*time.Second)
	start, err = protocol.DecodePayload[protocol.TaskStartPayload](env)
	if err != nil || start.UseGitBash {
		t.Fatalf("dispatched terminal UseGitBash = %v (err=%v), want false", start.UseGitBash, err)
	}

	// Disable via REST API.
	req, _ := http.NewRequest(http.MethodPut, th.ts.URL+"/api/workers/w-win-gb/agents-use-git-bash", strings.NewReader(`{"agents_use_git_bash":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Syre-Request", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT agents-use-git-bash: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT agents-use-git-bash status = %d, want 200", resp.StatusCode)
	}

	// Setting survives a subsequent UpsertWorker (worker reconnect).
	if err := th.st.UpsertWorker(ctx, store.Worker{
		ID: "w-win-gb", Name: "winbox", HostGroup: "winbox", OS: "windows", Arch: "amd64",
		Agents: []string{"claude", "shell"}, MaxConcurrent: 4, Status: "online", LastSeenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	wk, err = th.st.GetWorker(ctx, "w-win-gb")
	if err != nil || wk.AgentsUseGitBash {
		t.Fatalf("stored AgentsUseGitBash after upsert = %v (err=%v), want false", wk.AgentsUseGitBash, err)
	}

	// Subsequent agent dispatch receives UseGitBash=false.
	_, err = th.svc.DispatchTask(ctx, DispatchRequest{
		WorkerSelector: WorkerSelector{WorkerID: "w-win-gb"},
		SessionKind:    "chat",
		Agent:          "claude",
		Prompt:         "hello again",
		RepoPath:       "~",
		Mode:           protocol.TaskModeInteractive,
	})
	if err != nil {
		t.Fatalf("dispatch claude off: %v", err)
	}
	env = fw.expect(protocol.MsgTaskStart, 3*time.Second)
	start, err = protocol.DecodePayload[protocol.TaskStartPayload](env)
	if err != nil || start.UseGitBash {
		t.Fatalf("dispatched claude UseGitBash = %v (err=%v), want false", start.UseGitBash, err)
	}

	// Chat open payload reflects UseGitBash=false after disabling.
	openOff, err := th.h.buildChatOpen(ctx, chatSess)
	if err != nil || openOff.UseGitBash {
		t.Fatalf("buildChatOpen after disable UseGitBash = %v (err=%v), want false", openOff.UseGitBash, err)
	}

	// FleetStatus exposes agents_use_git_bash.
	fs, err := th.svc.FleetStatus(ctx)
	if err != nil || len(fs.Workers) != 1 {
		t.Fatalf("FleetStatus = %+v, %v", fs, err)
	}
	b, _ := json.Marshal(fs.Workers[0], protocol.JSONOptions)
	if !strings.Contains(string(b), `"agents_use_git_bash":false`) {
		t.Fatalf("worker view JSON missing agents_use_git_bash: %s", b)
	}
}
