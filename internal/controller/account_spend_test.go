package controller

import (
	"context"
	"encoding/json/v2"
	"math"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// An API-key account shows what its chats cost instead of subscription windows; a
// subscription account (interactive or long-lived token) shows none (SYR-BA6HV).
func TestAPIKeyAccountShowsSpend(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	cs.connectWorker(t, testWorkerInfo("w-1", "box-1"), nil, "w-1")
	put := func(id, method string) {
		t.Helper()
		raw, _ := json.Marshal(protocol.AgentAccount{ID: id, Agent: "claude", Label: id, Auth: protocol.AgentAuthStatus{State: protocol.AuthOK, Method: method}}, protocol.JSONOptions)
		if err := cs.st.UpsertAgentAccount(ctx, store.AgentAccountRecord{WorkerID: "w-1", AccountID: id, JSON: string(raw), UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	put("api", "api_key")
	put("token", "setup_token")
	now := time.Now().UTC()
	if err := cs.st.CreateChatSession(ctx, store.ChatSession{ID: "c-api", Backend: store.ChatBackendAgent, WorkerID: "w-1", Agent: "claude", AccountID: "api", Status: protocol.ChatStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for i, cost := range []float64{0.25, 0.5} {
		raw, _ := json.Marshal(protocol.AgentChatEvent{Kind: protocol.EventUsage, Usage: &protocol.AgentTokenUsage{InputTokens: 10, CostUSD: cost}}, protocol.JSONOptions)
		if _, err := cs.st.AppendChatEvent(ctx, store.ChatEvent{ChatID: "c-api", Seq: int64(i + 1), Kind: protocol.EventUsage, JSON: string(raw), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	views, err := cs.h.agentAccountViews(ctx, "w-1")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]AgentAccountView{}
	for _, v := range views {
		byID[v.ID] = v
	}
	if sp := byID["api"].Spend; sp == nil || math.Abs(sp.TodayUSD-0.75) > 1e-9 || math.Abs(sp.MonthUSD-0.75) > 1e-9 {
		t.Fatalf("api spend = %+v", sp)
	}
	if byID["token"].Spend != nil {
		t.Fatal("a token (subscription) account shows spend")
	}
}
