package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestCloseTerminalHangsUpAndPlacesNoHold(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	info := testWorkerInfo("w1", "box")
	info.Agents = append(info.Agents, "shell")
	fw := cs.connectWorker(t, info, nil, "w1")
	term := startTask(t, cs.svc, fw, userCtx(), "w1", "", func(r *DispatchRequest) {
		r.Agent, r.SessionKind, r.Mode = "shell", "terminal", protocol.TaskModeInteractive
	})
	code, raw := cs.api(t, http.MethodPost, "/api/tasks/"+term.ID+"/close", nil)
	if code != http.StatusOK {
		t.Fatalf("close: %d %s", code, raw)
	}
	p, err := protocol.DecodePayload[protocol.TaskCancelPayload](fw.expect(protocol.MsgTaskCancel, 3*time.Second))
	if err != nil || p.TaskID != term.ID || !p.Hangup {
		t.Fatalf("cancel payload: %+v %v", p, err)
	}
	if holds, _ := cs.st.ListHolds(ctx, store.HoldFilter{ActiveOnly: true, Limit: 10}); len(holds) != 0 {
		t.Fatalf("closing a terminal placed a hold: %+v", holds)
	}
	if a, _ := cs.st.ListAudit(ctx, store.AuditFilter{Action: AuditTaskClose, Limit: 5}); len(a) != 1 {
		t.Fatalf("audit: %+v", a)
	}

	// An agent session is never closed this way: closing its view leaves it running.
	agent := startTask(t, cs.svc, fw, userCtx(), "w1", "work", nil)
	code, raw = cs.api(t, http.MethodPost, "/api/tasks/"+agent.ID+"/close", nil)
	if code != http.StatusBadRequest || !strings.Contains(string(raw), "only a terminal") {
		t.Fatalf("closing an agent session: %d %s", code, raw)
	}
	if got := fw.drain(protocol.MsgTaskCancel, 300*time.Millisecond); len(got) != 0 {
		t.Fatal("an agent session was canceled")
	}
}
