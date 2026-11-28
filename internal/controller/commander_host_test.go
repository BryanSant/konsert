package controller

import (
	"net/http"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

func TestCommanderChatsRunOnTheChosenMachine(t *testing.T) {
	cs := newChatServer(t) // the hub's hostname is "hub-host"
	cs.connectWorker(t, testWorkerInfo("w-hub", "hub-host"), nil, "w-hub")
	fw2 := cs.connectWorker(t, testWorkerInfo("w-2", "box-2"), nil, "w-2")

	create := func(fleetTools bool) store.ChatSession {
		t.Helper()
		code, raw := cs.api(t, http.MethodPost, "/api/chats", map[string]any{"worker_id": "w-hub", "agent": "mock", "fleet_tools": fleetTools})
		if code != http.StatusOK && code != http.StatusCreated {
			t.Fatalf("create chat: %d %s", code, raw)
		}
		return decodeInto[store.ChatSession](t, raw)
	}

	// Default: the hub's machine.
	if c := create(true); c.WorkerID != "w-hub" {
		t.Fatalf("default Commander host = %q", c.WorkerID)
	}
	// Settings → Commander → Runs on: new Commander chats start there; agent chats don't move.
	if code, raw := cs.api(t, http.MethodPut, "/api/settings", map[string]any{"commander_worker_id": "box-2"}); code != http.StatusOK ||
		decodeInto[settingsResponse](t, raw).CommanderWorkerID != "w-2" {
		t.Fatalf("set Runs on: %d %s", code, raw)
	}
	if code, _ := cs.api(t, http.MethodPut, "/api/settings", map[string]any{"commander_worker_id": "nope"}); code != http.StatusBadRequest {
		t.Fatalf("unknown machine accepted: %d", code)
	}
	if c := create(true); c.WorkerID != "w-2" {
		t.Fatalf("Commander chat on %q, want w-2", c.WorkerID)
	}
	if c := create(false); c.WorkerID != "w-hub" {
		t.Fatalf("agent chat moved to %q", c.WorkerID)
	}
	// The chosen machine offline: the hub's machine, and the hub says it fell back.
	fw2.close()
	waitFor(t, 3*time.Second, "w-2 disconnected", func() bool { return !cs.h.IsWorkerConnected("w-2") })
	if c := create(true); c.WorkerID != "w-hub" {
		t.Fatalf("fallback Commander host = %q", c.WorkerID)
	}
	if host, fell := cs.h.commanderHost(t.Context()); host != "w-hub" || !fell {
		t.Fatalf("commanderHost = %q, %v", host, fell)
	}
}
