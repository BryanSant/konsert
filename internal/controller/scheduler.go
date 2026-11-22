package controller

import (
	"slices"
	"sort"

	"github.com/BryanSant/konsert/internal/store"
)

// WorkerSelector filters eligible worker nodes for a dispatched task.
type WorkerSelector struct {
	WorkerID  string            `json:"worker_id,omitempty"`
	HostGroup string            `json:"host_group,omitempty"`
	OS        string            `json:"os,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
}

// SelectWorker chooses the best online worker with available capacity matching
// selector, agent capability, tags, and permission compatibility.
func SelectWorker(
	workers []store.Worker,
	activeCounts map[string]int,
	selector WorkerSelector,
	agent string,
	permMode store.PermissionMode,
) (store.Worker, bool) {
	if permMode == "" {
		permMode = store.PermissionFullAccess
	}

	type candidate struct {
		worker store.Worker
		util   float64
	}
	var candidates []candidate

	for _, w := range workers {
		if w.Status != "online" {
			continue
		}
		maxC := w.MaxConcurrent
		if maxC <= 0 {
			maxC = 1
		}
		active := activeCounts[w.ID]
		if active >= maxC {
			continue
		}
		if selector.WorkerID != "" && w.ID != selector.WorkerID {
			continue
		}
		if selector.HostGroup != "" && w.HostGroup != selector.HostGroup {
			continue
		}
		if selector.OS != "" && w.OS != selector.OS {
			continue
		}
		if agent != "" && !slices.Contains(w.Agents, agent) {
			continue
		}
		// A sandboxed worker refuses full_access escalation.
		if w.PermissionMode == store.PermissionSandboxed && permMode == store.PermissionFullAccess {
			continue
		}
		if !tagsMatch(w.Tags, selector.Tags) {
			continue
		}

		candidates = append(candidates, candidate{
			worker: w,
			util:   float64(active) / float64(maxC),
		})
	}

	if len(candidates) == 0 {
		return store.Worker{}, false
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].util != candidates[j].util {
			return candidates[i].util < candidates[j].util
		}
		if !candidates[i].worker.LastSeenAt.Equal(candidates[j].worker.LastSeenAt) {
			return candidates[i].worker.LastSeenAt.After(candidates[j].worker.LastSeenAt)
		}
		return candidates[i].worker.ID < candidates[j].worker.ID
	})

	return candidates[0].worker, true
}

func tagsMatch(workerTags, required map[string]string) bool {
	for k, v := range required {
		if workerTags == nil {
			return false
		}
		if workerTags[k] != v {
			return false
		}
	}
	return true
}
