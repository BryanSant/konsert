package controller

import (
	"github.com/BryanSant/syre/internal/capacity"
	"github.com/BryanSant/syre/internal/placement"
	"slices"
	"strconv"
	"strings"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// WorkerSelector filters eligible worker nodes using both identity/tags and cached Machine Facts.
type WorkerSelector struct {
	WorkerID       string            `json:"worker_id,omitempty"`
	Name           string            `json:"name,omitempty"` // named host match (e.g. "foo1")
	HostGroup      string            `json:"host_group,omitempty"`
	OS             string            `json:"os,omitempty"`             // "darwin"/"macos", "linux", "windows"
	Arch           string            `json:"arch,omitempty"`           // "amd64", "arm64", ...
	Virtualization string            `json:"virtualization,omitempty"` // "bare_metal" | "wsl" | "container" | "mac_container" | "vm"
	RequiredTools  map[string]string `json:"required_tools,omitempty"` // e.g. {"postgres": "18", "gcloud": ""}
	RequiredAuth   []string          `json:"required_auth,omitempty"`  // e.g. ["gcloud", "gh"]
	MinRAMMB       uint64            `json:"min_ram_mb,omitzero"`
	MinDiskFreeGB  uint64            `json:"min_disk_free_gb,omitzero"`
	TmpTmpfs       *bool             `json:"tmp_tmpfs,omitzero"`
	Tags           map[string]string `json:"tags,omitempty"`
	// GPU requires a GPU for compute work (not a small integrated one), of GPUVendor
	// (nvidia, amd, intel, apple) when set, with at least MinVRAMGB of memory (SYR-FSQP8).
	GPU       bool   `json:"gpu,omitzero"`
	GPUVendor string `json:"gpu_vendor,omitempty"`
	MinVRAMGB int    `json:"min_vram_gb,omitzero"`
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
	return SelectWorkerWithLocality(workers, activeCounts, selector, agent, permMode, nil)
}

// LocalityScoreFn scores a worker's repo checkout locality for a ticket.
// It returns (matchedRepos, hasAll, eligible): workers with eligible=false are skipped;
// otherwise higher hasAll and matchedRepos rank ahead of utilization and disk space.
type LocalityScoreFn func(w store.Worker) (matched int, hasAll bool, eligible bool)

// SelectWorkerWithLocality is SelectWorker with optional checkout-locality preference.
func SelectWorkerWithLocality(
	workers []store.Worker,
	activeCounts map[string]int,
	selector WorkerSelector,
	agent string,
	permMode store.PermissionMode,
	locality LocalityScoreFn,
) (store.Worker, bool) {
	w, _, ok := SelectWorkerScored(workers, activeCounts, selector, agent, permMode, PlacementOpts{Locality: locality})
	return w, ok
}

// PlacementOpts feeds the placement scorer (SYR-YCQQ1): checkout locality, each
// machine's headroom, and the work (its expected peak, and a seed that makes the
// rotation of unknown work deterministic).
type PlacementOpts struct {
	Locality LocalityScoreFn
	Headroom func(w store.Worker) *capacity.Headroom
	Work     placement.Work
	// Rules are the placement rules for the work's scope (SYR-880CP); ClassOf gives a
	// machine's class for class targets.
	Rules   []store.PlacementRule
	ClassOf func(w store.Worker) string
}

// Rejection is a machine the hard filters or a require rule ruled out, and why.
type Rejection struct {
	WorkerID   string `json:"worker_id"`
	WorkerName string `json:"worker_name"`
	Reason     string `json:"reason"`
}

// PlacementOutcome is what a placement saw: the candidates ranked best first, the
// machines ruled out, and the rules that applied.
type PlacementOutcome struct {
	Ranked   []placement.Scored    `json:"ranked"`
	Rejected []Rejection           `json:"rejected,omitempty"`
	Rules    []store.PlacementRule `json:"rules,omitempty"`
}

// ruleTargets reports whether rule r's target is machine w.
func ruleTargets(r store.PlacementRule, w store.Worker, class func(store.Worker) string) bool {
	t := strings.TrimSpace(r.Target)
	switch r.TargetKind {
	case store.RuleTargetMachine:
		return strings.EqualFold(w.ID, t) || strings.EqualFold(w.SID, t) || strings.EqualFold(w.Name, t)
	case store.RuleTargetClass:
		return class != nil && t != "" && strings.HasPrefix(class(w), t)
	}
	return false
}

// placementWork is the scorer's view of work expected to peak at peakMB (0: no profile
// yet) placed for seed.
func placementWork(peakMB uint64, seed string) placement.Work {
	return placement.Work{PeakMB: peakMB, Known: peakMB > 0, Seed: seed}
}

// SelectWorkerScored applies the hard filters (the selector: requirements, agent,
// slots, permission mode; locality's eligibility), then ranks the rest with the
// placement scorer and picks one. It also returns the ranking (best first), for
// explanations.
func SelectWorkerScored(
	workers []store.Worker,
	activeCounts map[string]int,
	selector WorkerSelector,
	agent string,
	permMode store.PermissionMode,
	opts PlacementOpts,
) (store.Worker, PlacementOutcome, bool) {
	if permMode == "" {
		permMode = store.PermissionFullAccess
	}
	var out PlacementOutcome
	var requires []store.PlacementRule
	for _, r := range opts.Rules {
		if r.Effect == store.RuleEffectRequire {
			requires = append(requires, r)
		}
	}
	out.Rules = opts.Rules
	byID := map[string]store.Worker{}
	var cands []placement.Candidate
	reject := func(w store.Worker, why string) {
		out.Rejected = append(out.Rejected, Rejection{WorkerID: w.ID, WorkerName: w.Name, Reason: why})
	}
	for _, w := range workers {
		if why := selectorMismatch(w, activeCounts, selector, agent, permMode); why != "" {
			reject(w, why)
			continue
		}
		// Require rules narrow the machines the policy admitted (any of their targets).
		if len(requires) > 0 && !slices.ContainsFunc(requires, func(r store.PlacementRule) bool { return ruleTargets(r, w, opts.ClassOf) }) {
			reject(w, "a placement rule requires "+requires[0].Target)
			continue
		}
		c := placement.Candidate{ID: w.ID, Active: activeCounts[w.ID], Slots: max(w.MaxConcurrent, 1), LastSeen: w.LastSeenAt}
		if opts.Locality != nil {
			matched, hasAll, ok := opts.Locality(w)
			if !ok {
				reject(w, "no checkout, and a bound repo has no remote to clone")
				continue
			}
			c.LocalityMatched, c.LocalityTotal = matched, matched
			if !hasAll {
				c.LocalityTotal = matched + 1 // at least one bound repo is missing here
			}
		}
		for _, r := range opts.Rules {
			if !ruleTargets(r, w, opts.ClassOf) {
				continue
			}
			switch r.Effect {
			case store.RuleEffectPrefer:
				c.RuleBonus += placement.RuleBonus
			case store.RuleEffectAvoid:
				c.RuleBonus -= placement.RuleBonus
			}
		}
		if f := decodeWorkerFacts(w.FactsJSON); f != nil {
			c.DiskFreeGB = f.DiskFreeGB
		}
		if opts.Headroom != nil {
			c.Headroom = opts.Headroom(w)
		}
		byID[w.ID] = w
		cands = append(cands, c)
	}
	best, ranked, ok := placement.Choose(opts.Work, cands)
	out.Ranked = ranked
	if !ok {
		return store.Worker{}, out, false
	}
	return byID[best.ID], out, true
}

func workerMatchesSelector(
	w store.Worker,
	activeCounts map[string]int,
	selector WorkerSelector,
	agent string,
	permMode store.PermissionMode,
) bool {
	return selectorMismatch(w, activeCounts, selector, agent, permMode) == ""
}

// selectorMismatch is why w can't take the work ("" when it can).
func selectorMismatch(
	w store.Worker,
	activeCounts map[string]int,
	selector WorkerSelector,
	agent string,
	permMode store.PermissionMode,
) string {
	if w.Status != "online" {
		return "offline"
	}
	maxC := w.MaxConcurrent
	if maxC <= 0 {
		maxC = 1
	}
	if activeCounts[w.ID] >= maxC {
		return "every slot is busy"
	}
	if selector.WorkerID != "" && w.ID != selector.WorkerID {
		return "another machine was asked for"
	}
	if selector.Name != "" {
		want := strings.TrimSpace(selector.Name)
		if !strings.EqualFold(w.Name, want) && !strings.EqualFold(w.HostGroup, want) && !strings.EqualFold(w.ID, want) {
			return "another machine was asked for"
		}
	}
	if selector.HostGroup != "" && !strings.EqualFold(w.HostGroup, selector.HostGroup) {
		return "not in host group " + selector.HostGroup
	}
	if selector.OS != "" && normalizeOSName(w.OS) != normalizeOSName(selector.OS) {
		return "requires " + normalizeOSName(selector.OS)
	}
	if selector.Arch != "" && !strings.EqualFold(w.Arch, strings.TrimSpace(selector.Arch)) {
		return "requires " + strings.TrimSpace(selector.Arch)
	}
	if agent != "" && !slices.Contains(w.Agents, agent) {
		return "no " + agent + " here"
	}
	// A sandboxed worker refuses full_access escalation.
	if w.PermissionMode == store.PermissionSandboxed && permMode == store.PermissionFullAccess {
		return "sandboxed: no full access"
	}
	if !tagsMatch(w.Tags, selector.Tags) {
		return "tags don't match"
	}

	// Fact-based constraints (evaluated against cached MachineFacts).
	needsFacts := selector.Virtualization != "" ||
		len(selector.RequiredTools) > 0 ||
		len(selector.RequiredAuth) > 0 ||
		selector.MinRAMMB > 0 ||
		selector.MinDiskFreeGB > 0 ||
		selector.TmpTmpfs != nil ||
		selector.wantsGPU()

	if needsFacts {
		facts := decodeWorkerFacts(w.FactsJSON)
		if facts == nil {
			return "no machine facts yet"
		}
		if !factsMatchSelector(facts, selector) {
			return "doesn't meet the requirements"
		}
	}
	return ""
}

// wantsGPU reports a GPU requirement.
func (sel WorkerSelector) wantsGPU() bool {
	return sel.GPU || strings.TrimSpace(sel.GPUVendor) != "" || sel.MinVRAMGB > 0
}

// hasGPUFor reports a GPU in f that meets sel's GPU requirement.
func hasGPUFor(f *protocol.MachineFacts, sel WorkerSelector) bool {
	vendor := strings.ToLower(strings.TrimSpace(sel.GPUVendor))
	for _, g := range f.GPUs {
		if g.Integrated || vendor != "" && g.Vendor != vendor {
			continue
		}
		if sel.MinVRAMGB > 0 && g.VRAMMB < uint64(sel.MinVRAMGB)*1024 {
			continue
		}
		return true
	}
	return false
}

func factsMatchSelector(f *protocol.MachineFacts, sel WorkerSelector) bool {
	if sel.wantsGPU() && !hasGPUFor(f, sel) {
		return false
	}
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
