package observe

import "sync"

// ManagedSet records sessions Syre runs itself, by agent and agent session id, as the
// daemon learns them: a chat's session_ready, a hook fired inside a managed task
// (SYRE_TASK_ID). Its Has is a Config.Managed predicate, alone or combined with others
// (a managed PID, say).
type ManagedSet struct {
	mu sync.Mutex
	m  map[string]struct{}
}

// maxManaged bounds the set; ids of long-finished sessions fall out first in effect
// (the set is cleared), and those sessions are idle and pruned by then.
const maxManaged = 8192

// Mark records agent's session id as managed; agent "" marks it for any agent (agent
// session ids are UUIDs, unique across agents).
func (ms *ManagedSet) Mark(agent, sessionID string) {
	if sessionID == "" {
		return
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.m == nil || len(ms.m) >= maxManaged {
		ms.m = map[string]struct{}{}
	}
	ms.m[agent+"/"+sessionID] = struct{}{}
}

// Has reports whether s was marked managed.
func (ms *ManagedSet) Has(s Session) bool {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	_, ok := ms.m[s.Agent+"/"+s.ID]
	if !ok {
		_, ok = ms.m["/"+s.ID]
	}
	return ok
}
