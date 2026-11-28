package controller

import (
	"context"
	"encoding/json/v2"
	"maps"
	"regexp"
	"strings"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Work keys and declared requirements (SYR-2BE8M; docs/prd.md §5.4). A task's work
// key is set at dispatch: project / epic or ticket / repo, plus "/<kind>" when the
// dispatcher names one (build, test, ml-train, review, …), or the dispatcher's own key.
// Requirements are hard constraints on the machine; declared for a work key, they are
// remembered and inherited by later work of that key that declares none.

// WorkRequirements are hard requirements on the machine a task runs on.
type WorkRequirements struct {
	OS   string `json:"os,omitempty" jsonschema:"darwin, linux or windows"`
	Arch string `json:"arch,omitempty" jsonschema:"amd64, arm64, …"`
	// MinRAMGB is the machine's total memory; MinDiskFreeGB its free disk.
	MinRAMGB      int `json:"min_ram_gb,omitzero"`
	MinDiskFreeGB int `json:"min_disk_free_gb,omitzero"`
	// Tools maps a tool or SDK to its minimum version ("" any): {"go": "1.26", "docker": ""}.
	Tools map[string]string `json:"tools,omitempty"`
	// Auth lists logins the machine must have ready ("gh", "gcloud").
	Auth []string `json:"auth,omitempty"`
	// Virtualization is bare_metal, wsl, container, mac_container or vm.
	Virtualization string `json:"virtualization,omitempty"`
}

func (r WorkRequirements) empty() bool {
	return r.OS == "" && r.Arch == "" && r.MinRAMGB <= 0 && r.MinDiskFreeGB <= 0 && len(r.Tools) == 0 &&
		len(r.Auth) == 0 && r.Virtualization == ""
}

// apply adds the requirements to sel; what the selector already names wins.
func (r WorkRequirements) apply(sel *WorkerSelector) {
	if sel.OS == "" {
		sel.OS = strings.TrimSpace(r.OS)
	}
	if sel.Arch == "" {
		sel.Arch = strings.TrimSpace(r.Arch)
	}
	if sel.MinRAMMB == 0 && r.MinRAMGB > 0 {
		sel.MinRAMMB = uint64(r.MinRAMGB) * 1024
	}
	if sel.MinDiskFreeGB == 0 && r.MinDiskFreeGB > 0 {
		sel.MinDiskFreeGB = uint64(r.MinDiskFreeGB)
	}
	if len(r.Tools) > 0 {
		if sel.RequiredTools == nil {
			sel.RequiredTools = map[string]string{}
		}
		for k, v := range r.Tools {
			if _, set := sel.RequiredTools[k]; !set && strings.TrimSpace(k) != "" {
				sel.RequiredTools[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	for _, a := range r.Auth {
		if a = strings.TrimSpace(a); a != "" && !containsFold(sel.RequiredAuth, a) {
			sel.RequiredAuth = append(sel.RequiredAuth, a)
		}
	}
	if sel.Virtualization == "" {
		sel.Virtualization = strings.TrimSpace(r.Virtualization)
	}
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

var workKindRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// normalizeWorkKind is kind lower-cased; ok is false for one that can't be a key part.
func normalizeWorkKind(kind string) (string, bool) {
	k := strings.ToLower(strings.TrimSpace(kind))
	if k == "" {
		return "", true
	}
	return k, workKindRe.MatchString(k)
}

// taskWorkKey is the work key of a task about to be created: override when the
// dispatcher gave one, else the derived key with the kind appended.
func (h *Hub) taskWorkKey(ctx context.Context, ticketID string, task store.Task, kind, override string) string {
	if k := strings.TrimSpace(override); k != "" {
		return k
	}
	key := h.workKeyFor(ctx, store.Attempt{TicketID: ticketID}, task)
	if kind != "" {
		key += "/" + kind
	}
	return key
}

// workRequirements resolves the requirements of work with key: the declared ones
// (remembered for the key), else those remembered for it. inherited reports the latter.
func (h *Hub) workRequirements(ctx context.Context, key string, declared *WorkRequirements, setBy string) (req WorkRequirements, inherited bool) {
	if declared != nil && !declared.empty() {
		r := *declared
		r.Tools = maps.Clone(r.Tools)
		if raw, err := json.Marshal(r, protocol.JSONOptions); err == nil && key != "" {
			if err := h.store.SetWorkRequirements(ctx, key, string(raw), setBy); err != nil {
				h.logger.Warn("work requirements not stored", "work_key", key, "err", err)
			}
		}
		return r, false
	}
	if key == "" {
		return WorkRequirements{}, false
	}
	raw, err := h.store.GetWorkRequirements(ctx, key)
	if err != nil {
		return WorkRequirements{}, false
	}
	if json.Unmarshal([]byte(raw), &req, protocol.JSONOptions) != nil || req.empty() {
		return WorkRequirements{}, false
	}
	return req, true
}
