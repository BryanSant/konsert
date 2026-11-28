package controller

import (
	"context"
	"encoding/json/v2"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// What an API-key account spent (SYR-BA6HV): pay-per-token accounts have no
// subscription windows, so the usage popover shows the cost their chats reported today
// and this month instead. Summed from the chats' usage events, cached for a few minutes.

const accountSpendTTL = 5 * time.Minute

// AccountSpend is an API-key account's cost so far.
type AccountSpend struct {
	TodayUSD float64   `json:"today_usd"`
	MonthUSD float64   `json:"month_usd"`
	AsOf     time.Time `json:"as_of"`
}

type spendCache struct {
	mu sync.Mutex
	m  map[string]AccountSpend
}

// accountSpend sums the cost of workerID's chats on accountID since the start of this
// month (and of today), in the hub's local time.
func (h *Hub) accountSpend(ctx context.Context, workerID, accountID string) AccountSpend {
	key := workerID + "/" + accountID
	now := time.Now()
	h.spend.mu.Lock()
	if s, ok := h.spend.m[key]; ok && now.Sub(s.AsOf) < accountSpendTTL {
		h.spend.mu.Unlock()
		return s
	}
	h.spend.mu.Unlock()

	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	out := AccountSpend{AsOf: now}
	chats, _ := h.store.ListChatSessionsByWorker(ctx, workerID)
	for _, cs := range chats {
		if cs.AccountID != accountID || cs.UpdatedAt.Before(monthStart) {
			continue
		}
		evs, err := h.store.ListChatEvents(ctx, cs.ID, 0, 5000)
		if err != nil {
			continue
		}
		for _, e := range evs {
			if e.Kind != protocol.EventUsage || e.CreatedAt.Before(monthStart) {
				continue
			}
			var ev protocol.AgentChatEvent
			if json.Unmarshal([]byte(e.JSON), &ev, protocol.JSONOptions) != nil || ev.Usage == nil {
				continue
			}
			out.MonthUSD += ev.Usage.CostUSD
			if !e.CreatedAt.Before(dayStart) {
				out.TodayUSD += ev.Usage.CostUSD
			}
		}
	}
	h.spend.mu.Lock()
	if h.spend.m == nil {
		h.spend.m = map[string]AccountSpend{}
	}
	h.spend.m[key] = out
	h.spend.mu.Unlock()
	return out
}
