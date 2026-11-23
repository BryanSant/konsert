package chat

// Live tests against the real, logged-in agent CLIs. Opt in with KONSERT_LIVE_ACP=1;
// KONSERT_LIVE_ACP_AGENTS=grok,agy limits the agents. Each agent gets ONE tiny prompt;
// KONSERT_LIVE_ACP_ELICIT=<agent> additionally runs one permission round trip.
// These use the real home directory only through the CLIs themselves and remove the
// session files they create.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

func liveAgents(t *testing.T) []string {
	if os.Getenv("KONSERT_LIVE_ACP") != "1" {
		t.Skip("set KONSERT_LIVE_ACP=1 to run live ACP tests")
	}
	if v := os.Getenv("KONSERT_LIVE_ACP_AGENTS"); v != "" {
		return strings.Split(v, ",")
	}
	return ACPAgents()
}

func liveAccount(t *testing.T, agent string) Account {
	t.Helper()
	bin, err := exec.LookPath(acpAdapters[agent].DefaultBinary)
	if err != nil {
		t.Skipf("%s not installed", agent)
	}
	acct := Account{ID: agent, Agent: agent, Label: acpAdapters[agent].Label, Binary: bin, IsDefault: true}
	// KONSERT_LIVE_ACP_ENV="K=V,K2=V2" adds account env (experiments).
	if v := os.Getenv("KONSERT_LIVE_ACP_ENV"); v != "" {
		acct.Env = map[string]string{}
		for _, kv := range strings.Split(v, ",") {
			if k, val, ok := strings.Cut(kv, "="); ok {
				acct.Env[k] = val
			}
		}
	}
	return acct
}

func liveScratch(t *testing.T, agent string) string {
	t.Helper()
	dir, err := ScratchDir(acpProbePrefix + "live-" + agent + "-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func logEvents(t *testing.T, evs []protocol.AgentChatEvent) {
	t.Helper()
	for _, ev := range evs {
		switch ev.Kind {
		case protocol.EventMessageDelta, protocol.EventThoughtDelta:
			t.Logf("  %s %q", ev.Kind, truncate(ev.Text, 80))
		default:
			t.Logf("  %s", strings.TrimPrefix(dumpEvents([]protocol.AgentChatEvent{ev}), "\n  "))
		}
	}
}

func TestLiveACPProbe(t *testing.T) {
	for _, agent := range liveAgents(t) {
		t.Run(agent, func(t *testing.T) {
			acct := liveAccount(t, agent)
			eng, _ := NewACPEngine(agent)
			start := time.Now()
			got := eng.Probe(context.Background(), acct)
			var models []string
			for _, m := range got.Models {
				models = append(models, m.ID+"["+effortIDs(m)+"]")
			}
			t.Logf("probe %s in %s: version=%s auth=%+v plan=%q default=%s source=%s models(%d)=%s",
				agent, time.Since(start).Round(time.Millisecond), got.Version, got.Auth, got.Plan, got.DefaultModel, got.ModelsSource, len(models), truncate(strings.Join(models, " "), 600))
			if got.Auth.State != protocol.AuthOK || len(got.Models) == 0 {
				t.Errorf("probe failed: %+v", got.Auth)
			}
		})
	}
}

func TestLiveACPPrompt(t *testing.T) {
	for _, agent := range liveAgents(t) {
		t.Run(agent, func(t *testing.T) {
			acct := liveAccount(t, agent)
			dir := liveScratch(t, agent)
			eng, _ := NewACPEngine(agent)
			rec := newRecorder()
			s, err := eng.Open(context.Background(), acct, protocol.AgentChatOpen{ChatID: "live", Agent: agent, Cwd: dir}, rec.emit)
			if err != nil {
				logEvents(t, rec.all())
				t.Fatalf("Open: %v", err)
			}
			sid := s.AgentSessionID()
			t.Cleanup(func() {
				_ = s.Close()
				removed := acpAdapters[agent].removeSessionFiles(acct, sid, dir)
				t.Logf("removed session files: %v", removed)
			})
			if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "live-1", Text: "Reply with exactly: OK"}); err != nil {
				t.Fatal(err)
			}
			end := waitLive(t, rec, func(ev protocol.AgentChatEvent) bool {
				return ev.Kind == protocol.EventTurnEnd || ev.Kind == protocol.EventError
			})
			logEvents(t, rec.all())
			if end.Kind != protocol.EventTurnEnd || end.StopReason != "end_turn" {
				t.Fatalf("turn ended with %+v", end)
			}
			var msg string
			deltas := 0
			for _, ev := range rec.all() {
				switch ev.Kind {
				case protocol.EventMessageDelta:
					deltas++
				case protocol.EventMessage:
					msg += ev.Text
				}
			}
			if deltas == 0 || !strings.Contains(msg, "OK") {
				t.Errorf("deltas=%d message=%q", deltas, msg)
			}
		})
	}
}

func TestLiveACPElicitation(t *testing.T) {
	liveAgents(t)
	agent := os.Getenv("KONSERT_LIVE_ACP_ELICIT")
	if agent == "" {
		t.Skip("set KONSERT_LIVE_ACP_ELICIT=<agent> to run the permission round trip")
	}
	acct := liveAccount(t, agent)
	dir := liveScratch(t, agent)
	eng, _ := NewACPEngine(agent)
	rec := newRecorder()
	s, err := eng.Open(context.Background(), acct, protocol.AgentChatOpen{ChatID: "live", Agent: agent, Cwd: dir, ApprovalMode: os.Getenv("KONSERT_LIVE_ACP_MODE")}, rec.emit)
	if err != nil {
		logEvents(t, rec.all())
		t.Fatalf("Open: %v", err)
	}
	sid := s.AgentSessionID()
	t.Cleanup(func() {
		_ = s.Close()
		t.Logf("removed session files: %v", acpAdapters[agent].removeSessionFiles(acct, sid, dir))
	})
	prompt := "Create a file named hello.txt in the current directory containing exactly the text: hi. Use your file-writing tool, then reply DONE."
	if err := s.Prompt(context.Background(), protocol.AgentChatPrompt{TurnID: "live-e", Text: prompt}); err != nil {
		t.Fatal(err)
	}
	ev := waitLive(t, rec, func(ev protocol.AgentChatEvent) bool {
		return ev.Kind == protocol.EventElicitation || ev.Kind == protocol.EventTurnEnd || ev.Kind == protocol.EventError
	})
	if ev.Kind != protocol.EventElicitation {
		logEvents(t, rec.all())
		t.Fatalf("no permission request; got %+v", ev)
	}
	el := ev.Elicitation
	choice := ""
	for _, o := range el.Options {
		if o.Kind == "allow_once" {
			choice = o.ID
		}
	}
	if choice == "" && len(el.Options) > 0 {
		choice = el.Options[0].ID
	}
	if err := s.Respond(context.Background(), protocol.AgentChatRespond{ElicitationID: el.ID, OptionID: choice}); err != nil {
		t.Fatal(err)
	}
	end := waitLive(t, rec, func(ev protocol.AgentChatEvent) bool {
		if ev.Kind == protocol.EventElicitation && ev.Elicitation.ID != el.ID {
			// Approve follow-up requests for the same task too.
			_ = s.Respond(context.Background(), protocol.AgentChatRespond{ElicitationID: ev.Elicitation.ID, OptionID: "allow_once"})
		}
		return ev.Kind == protocol.EventTurnEnd || ev.Kind == protocol.EventError
	})
	logEvents(t, rec.all())
	if end.Kind != protocol.EventTurnEnd || end.StopReason != "end_turn" {
		t.Fatalf("turn ended with %+v", end)
	}
	b, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || !strings.Contains(string(b), "hi") {
		t.Errorf("hello.txt = %q, %v", b, err)
	}
}

func waitLive(t *testing.T, rec *evRecorder, pred func(protocol.AgentChatEvent) bool) protocol.AgentChatEvent {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	seen := 0
	for time.Now().Before(deadline) {
		evs := rec.all()
		for _, ev := range evs[seen:] {
			if pred(ev) {
				return ev
			}
		}
		seen = len(evs)
		time.Sleep(100 * time.Millisecond)
	}
	logEvents(t, rec.all())
	t.Fatal("live turn timed out")
	return protocol.AgentChatEvent{}
}
