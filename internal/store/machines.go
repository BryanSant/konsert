package store

import (
	"errors"
	"slices"
	"strings"
	"time"
)

// MachineGroup is a saved set of machines (SYR-FJA2T).
type MachineGroup struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	WorkerIDs []string  `json:"worker_ids"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Machine target kinds.
const (
	TargetAll      = "all"
	TargetMachine  = "machine"
	TargetMachines = "machines"
	TargetGroup    = "group"
	TargetLabel    = "label"
)

// MachineTarget names a set of machines: all of them, one, a list, a saved group, or every
// machine with an operator label (now and later). A Commander chat's scope is one. The web
// resolves it with the same rules (machine-target.ts); both check the shared vectors in
// web/src/app/shared/machines/machine-target.vectors.json.
type MachineTarget struct {
	Kind  string   `json:"kind"`
	ID    string   `json:"id,omitempty"`
	IDs   []string `json:"ids,omitempty"`
	Label string   `json:"label,omitempty"`
}

// IsAll reports whether t places no limit (the zero target is all machines).
func (t MachineTarget) IsAll() bool { return t.Kind == "" || t.Kind == TargetAll }

// Normalize trims t and checks it names something.
func (t MachineTarget) Normalize() (MachineTarget, error) {
	t.Kind = strings.TrimSpace(t.Kind)
	t.ID = strings.TrimSpace(t.ID)
	t.Label = strings.TrimSpace(t.Label)
	switch t.Kind {
	case "", TargetAll:
		return MachineTarget{Kind: TargetAll}, nil
	case TargetMachine, TargetGroup:
		if t.ID == "" {
			return t, errors.New(t.Kind + " target needs an id")
		}
		return MachineTarget{Kind: t.Kind, ID: t.ID}, nil
	case TargetMachines:
		var ids []string
		for _, id := range t.IDs {
			if id = strings.TrimSpace(id); id != "" && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return t, errors.New("machines target needs at least one id")
		}
		return MachineTarget{Kind: TargetMachines, IDs: ids}, nil
	case TargetLabel:
		if t.Label == "" {
			return t, errors.New("label target needs a label")
		}
		return MachineTarget{Kind: TargetLabel, Label: t.Label}, nil
	}
	return t, errors.New("unknown target kind " + t.Kind + " (all, machine, machines, group or label)")
}

// ResolveTarget returns the IDs of the machines t names right now, in workerIDs order.
// labels maps machine ID to its operator labels; groups maps group ID to its members.
// Labels match case-insensitively. A group or machine that no longer exists names nothing.
func ResolveTarget(t MachineTarget, workerIDs []string, labels map[string][]string, groups map[string][]string) []string {
	in := func(id string) bool {
		switch t.Kind {
		case "", TargetAll:
			return true
		case TargetMachine:
			return id == t.ID
		case TargetMachines:
			return slices.Contains(t.IDs, id)
		case TargetGroup:
			return slices.Contains(groups[t.ID], id)
		case TargetLabel:
			return slices.ContainsFunc(labels[id], func(l string) bool { return strings.EqualFold(l, t.Label) })
		}
		return false
	}
	out := []string{}
	for _, id := range workerIDs {
		if in(id) {
			out = append(out, id)
		}
	}
	return out
}

// NormalizeLabel trims a label and checks its length.
func NormalizeLabel(l string) (string, error) {
	l = strings.Join(strings.Fields(l), " ")
	if l == "" {
		return "", errors.New("label is empty")
	}
	if len([]rune(l)) > 40 {
		return "", errors.New("label is longer than 40 characters")
	}
	return l, nil
}
