package controller

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

// WorkerSelector filters eligible worker nodes using both identity/tags and cached Machine Facts.
type WorkerSelector struct {
	WorkerID       string            `json:"worker_id,omitempty"`
	Name           string            `json:"name,omitempty"` // named host match (e.g. "foo1")
	HostGroup      string            `json:"host_group,omitempty"`
	OS             string            `json:"os,omitempty"`             // "darwin"/"macos", "linux", "windows"
	Virtualization string            `json:"virtualization,omitempty"` // "bare_metal" | "wsl" | "container" | "mac_container" | "vm"
	RequiredTools  map[string]string `json:"required_tools,omitempty"` // e.g. {"postgres": "18", "gcloud": ""}
	RequiredAuth   []string          `json:"required_auth,omitempty"`  // e.g. ["gcloud", "gh"]
	MinRAMMB       uint64            `json:"min_ram_mb,omitempty"`
	MinDiskFreeGB  uint64            `json:"min_disk_free_gb,omitempty"`
	TmpTmpfs       *bool             `json:"tmp_tmpfs,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
}

func normalizeOSName(osName string) string {
	switch strings.ToLower(strings.TrimSpace(osName)) {
	case "mac", "macos", "osx", "darwin":
		return "darwin"
	case "win", "win32", "windows":
		return "windows"
	default:
		return strings.ToLower(strings.TrimSpace(osName))
	}
}

// SelectWorker chooses the best online worker with available capacity matching
// selector, cached machine facts/capabilities, agent availability, tags, and permission compatibility.
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
		worker     store.Worker
		util       float64
		diskFreeGB uint64
	}
	var candidates []candidate

	for _, w := range workers {
		if !workerMatchesSelector(w, activeCounts, selector, agent, permMode) {
			continue
		}
		maxC := w.MaxConcurrent
		if maxC <= 0 {
			maxC = 1
		}
		active := activeCounts[w.ID]
		var diskFree uint64
		if f := decodeWorkerFacts(w.FactsJSON); f != nil {
			diskFree = f.DiskFreeGB
		}
		candidates = append(candidates, candidate{
			worker:     w,
			util:       float64(active) / float64(maxC),
			diskFreeGB: diskFree,
		})
	}

	if len(candidates) == 0 {
		return store.Worker{}, false
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].util != candidates[j].util {
			return candidates[i].util < candidates[j].util
		}
		if candidates[i].diskFreeGB != candidates[j].diskFreeGB {
			return candidates[i].diskFreeGB > candidates[j].diskFreeGB
		}
		if !candidates[i].worker.LastSeenAt.Equal(candidates[j].worker.LastSeenAt) {
			return candidates[i].worker.LastSeenAt.After(candidates[j].worker.LastSeenAt)
		}
		return candidates[i].worker.ID < candidates[j].worker.ID
	})

	return candidates[0].worker, true
}

func workerMatchesSelector(
	w store.Worker,
	activeCounts map[string]int,
	selector WorkerSelector,
	agent string,
	permMode store.PermissionMode,
) bool {
	if w.Status != "online" {
		return false
	}
	maxC := w.MaxConcurrent
	if maxC <= 0 {
		maxC = 1
	}
	if activeCounts[w.ID] >= maxC {
		return false
	}
	if selector.WorkerID != "" && w.ID != selector.WorkerID {
		return false
	}
	if selector.Name != "" {
		want := strings.TrimSpace(selector.Name)
		if !strings.EqualFold(w.Name, want) && !strings.EqualFold(w.HostGroup, want) && !strings.EqualFold(w.ID, want) {
			return false
		}
	}
	if selector.HostGroup != "" && !strings.EqualFold(w.HostGroup, selector.HostGroup) {
		return false
	}
	if selector.OS != "" && normalizeOSName(w.OS) != normalizeOSName(selector.OS) {
		return false
	}
	if agent != "" && !slices.Contains(w.Agents, agent) {
		return false
	}
	// A sandboxed worker refuses full_access escalation.
	if w.PermissionMode == store.PermissionSandboxed && permMode == store.PermissionFullAccess {
		return false
	}
	if !tagsMatch(w.Tags, selector.Tags) {
		return false
	}

	// Fact-based constraints (evaluated against cached MachineFacts).
	needsFacts := selector.Virtualization != "" ||
		len(selector.RequiredTools) > 0 ||
		len(selector.RequiredAuth) > 0 ||
		selector.MinRAMMB > 0 ||
		selector.MinDiskFreeGB > 0 ||
		selector.TmpTmpfs != nil

	if needsFacts {
		facts := decodeWorkerFacts(w.FactsJSON)
		if facts == nil {
			return false
		}
		if !factsMatchSelector(facts, selector) {
			return false
		}
	}
	return true
}

func factsMatchSelector(f *protocol.MachineFacts, sel WorkerSelector) bool {
	if sel.Virtualization != "" && !strings.EqualFold(f.Virtualization, sel.Virtualization) {
		return false
	}
	if sel.MinRAMMB > 0 && f.RAMTotalMB < sel.MinRAMMB {
		return false
	}
	if sel.MinDiskFreeGB > 0 && f.DiskFreeGB < sel.MinDiskFreeGB {
		return false
	}
	if sel.TmpTmpfs != nil && f.TmpIsTmpfs != *sel.TmpTmpfs {
		return false
	}
	for tool, minVer := range sel.RequiredTools {
		gotVer, ok := lookupToolVersion(f.Tools, tool)
		if !ok {
			return false
		}
		if minVer != "" && !versionSatisfies(gotVer, minVer) {
			return false
		}
	}
	for _, authTool := range sel.RequiredAuth {
		if !lookupAuthReady(f.AuthReady, authTool) {
			return false
		}
	}
	return true
}

func lookupToolVersion(tools map[string]string, name string) (string, bool) {
	if len(tools) == 0 {
		return "", false
	}
	want := strings.ToLower(strings.TrimSpace(name))
	for k, v := range tools {
		if strings.ToLower(k) == want {
			return v, true
		}
	}
	// Alias postgresql <-> postgres
	if want == "postgresql" {
		if v, ok := tools["postgres"]; ok {
			return v, true
		}
	}
	return "", false
}

func lookupAuthReady(auth map[string]bool, name string) bool {
	if len(auth) == 0 {
		return false
	}
	want := strings.ToLower(strings.TrimSpace(name))
	for k, ok := range auth {
		if strings.ToLower(k) == want && ok {
			return true
		}
	}
	return false
}

// versionSatisfies checks if gotVer (e.g. "18.1") satisfies minVer (e.g. "18" or "18.0").
func versionSatisfies(gotVer, minVer string) bool {
	gotParts := parseVersionParts(gotVer)
	minParts := parseVersionParts(minVer)
	if len(gotParts) == 0 || len(minParts) == 0 {
		return strings.HasPrefix(strings.ToLower(gotVer), strings.ToLower(minVer))
	}
	for i := 0; i < len(minParts); i++ {
		g := 0
		if i < len(gotParts) {
			g = gotParts[i]
		}
		if g > minParts[i] {
			return true
		}
		if g < minParts[i] {
			return false
		}
	}
	return true
}

func parseVersionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	fields := strings.Split(v, ".")
	var out []int
	for _, f := range fields {
		// Strip trailing non-digits like "1-beta"
		digits := f
		for idx, r := range f {
			if r < '0' || r > '9' {
				digits = f[:idx]
				break
			}
		}
		if digits == "" {
			break
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
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
