package controller

import (
	"context"

	"github.com/BryanSant/syre/internal/store"
)

// commanderWorkerSetting is the machine new Commander chats run on (SYR-CNMDK).
const commanderWorkerSetting = "commander_worker_id"

// commanderHost is the machine a new Commander chat runs on: the one Settings → Commander
// names while it is connected, else the hub's own machine. fellBack reports that a chosen
// machine was offline. "" when neither is connected (the chat then picks like any other).
func (h *Hub) commanderHost(ctx context.Context) (workerID string, fellBack bool) {
	saved, _ := h.store.GetAllSettings(ctx)
	chosen := saved[commanderWorkerSetting]
	if chosen != "" && h.IsWorkerConnected(chosen) {
		return chosen, false
	}
	if local := h.hubMachine(ctx); local != "" {
		return local, chosen != "" && chosen != local
	}
	return "", chosen != ""
}

// hubMachine is the connected machine running on the hub's host, or "".
func (h *Hub) hubMachine(ctx context.Context) string {
	workers, err := h.store.ListWorkers(ctx)
	if err != nil {
		return ""
	}
	host := ""
	if h.hostname != nil {
		host = h.hostname()
	}
	var match string
	for _, w := range workers {
		if !h.IsWorkerConnected(w.ID) {
			continue
		}
		if w.Launch == store.LaunchLocal {
			return w.ID
		}
		if match == "" && isLocalhostWorker(w, host) {
			match = w.ID
		}
	}
	return match
}
