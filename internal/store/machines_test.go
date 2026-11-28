package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The web resolves targets with the same rules: both check these vectors.
func TestResolveTargetSharedVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "app", "shared", "machines", "machine-target.vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Machines []string            `json:"machines"`
		Labels   map[string][]string `json:"labels"`
		Groups   map[string][]string `json:"groups"`
		Cases    []struct {
			Name   string        `json:"name"`
			Target MachineTarget `json:"target"`
			Want   []string      `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range v.Cases {
		if got := ResolveTarget(c.Target, v.Machines, v.Labels, v.Groups); !slices.Equal(got, c.Want) {
			t.Errorf("%s: got %v, want %v", c.Name, got, c.Want)
		}
	}
}

func TestMachineTargetNormalize(t *testing.T) {
	for _, c := range []struct {
		in   MachineTarget
		want MachineTarget
		bad  bool
	}{
		{in: MachineTarget{}, want: MachineTarget{Kind: TargetAll}},
		{in: MachineTarget{Kind: "all", ID: "x"}, want: MachineTarget{Kind: TargetAll}},
		{in: MachineTarget{Kind: " machine ", ID: " m-1 "}, want: MachineTarget{Kind: TargetMachine, ID: "m-1"}},
		{in: MachineTarget{Kind: "machine"}, bad: true},
		{in: MachineTarget{Kind: "machines", IDs: []string{"a", " a", "", "b"}}, want: MachineTarget{Kind: TargetMachines, IDs: []string{"a", "b"}}},
		{in: MachineTarget{Kind: "machines", IDs: []string{" "}}, bad: true},
		{in: MachineTarget{Kind: "group"}, bad: true},
		{in: MachineTarget{Kind: "label", Label: " Work "}, want: MachineTarget{Kind: TargetLabel, Label: "Work"}},
		{in: MachineTarget{Kind: "label"}, bad: true},
		{in: MachineTarget{Kind: "cluster"}, bad: true},
	} {
		got, err := c.in.Normalize()
		if (err != nil) != c.bad {
			t.Errorf("%+v: err %v, want bad=%v", c.in, err, c.bad)
			continue
		}
		if !c.bad && (got.Kind != c.want.Kind || got.ID != c.want.ID || got.Label != c.want.Label || !slices.Equal(got.IDs, c.want.IDs)) {
			t.Errorf("%+v: got %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestNormalizeLabel(t *testing.T) {
	if l, err := NormalizeLabel("  Gaming   hardware "); err != nil || l != "Gaming hardware" {
		t.Errorf("got %q, %v", l, err)
	}
	if _, err := NormalizeLabel("   "); err == nil {
		t.Error("empty label accepted")
	}
	if _, err := NormalizeLabel("this label is far too long to be useful as a chip"); err == nil {
		t.Error("long label accepted")
	}
}
