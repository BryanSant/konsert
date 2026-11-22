package worker

import (
	"context"
	"sort"

	"github.com/BryanSant/konsert/internal/protocol"
)

// CollectMetrics gathers lightweight host CPU/memory/load and listening dev ports.
func CollectMetrics(ctx context.Context) *protocol.WorkerMetrics {
	m := collectPlatformMetrics(ctx)
	if m == nil {
		return &protocol.WorkerMetrics{}
	}
	// Deduplicate and sort ports
	if len(m.ListeningPorts) > 1 {
		seen := make(map[int]protocol.ListeningPort, len(m.ListeningPorts))
		for _, p := range m.ListeningPorts {
			if _, ok := seen[p.Port]; !ok {
				seen[p.Port] = p
			}
		}
		dedup := make([]protocol.ListeningPort, 0, len(seen))
		for _, p := range seen {
			dedup = append(dedup, p)
		}
		sort.Slice(dedup, func(i, j int) bool { return dedup[i].Port < dedup[j].Port })
		if len(dedup) > 32 {
			dedup = dedup[:32]
		}
		m.ListeningPorts = dedup
	}
	return m
}
