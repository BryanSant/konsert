package sqlite

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestMachineLabelsGroupsAndChatScope(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"w-a", "w-b", "w-c"} {
		if err := st.UpsertWorker(ctx, store.Worker{ID: id, Name: id, HostGroup: id, OS: "linux", Arch: "amd64", Status: "online"}); err != nil {
			t.Fatal(err)
		}
	}

	// Labels: case-insensitive duplicates are one label; unknown machines are not found.
	for _, l := range []string{"Work", "work", "Gaming hardware"} {
		if err := st.AddMachineLabel(ctx, "w-a", l); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AddMachineLabel(ctx, "w-b", "Work"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMachineLabel(ctx, "w-nope", "Work"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("label on an unknown machine: %v, want ErrNotFound", err)
	}
	labels, err := st.ListMachineLabels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(labels["w-a"], []string{"Work", "Gaming hardware"}) || !slices.Equal(labels["w-b"], []string{"Work"}) {
		t.Fatalf("labels = %v", labels)
	}
	if err := st.RemoveMachineLabel(ctx, "w-a", "GAMING HARDWARE"); err != nil {
		t.Fatal(err)
	}
	if labels, _ = st.ListMachineLabels(ctx); !slices.Equal(labels["w-a"], []string{"Work"}) {
		t.Fatalf("after remove: %v", labels["w-a"])
	}

	// Groups: create, unique names (any case), update members, delete.
	g, err := st.CreateMachineGroup(ctx, store.MachineGroup{Name: "Home lab", WorkerIDs: []string{"w-a", "w-c"}, CreatedBy: "bsant"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateMachineGroup(ctx, store.MachineGroup{Name: "home LAB"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate group name: %v, want ErrConflict", err)
	}
	if _, err := st.CreateMachineGroup(ctx, store.MachineGroup{Name: "Ghosts", WorkerIDs: []string{"w-nope"}}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("group with an unknown machine: %v, want ErrNotFound", err)
	}
	got, err := st.GetMachineGroup(ctx, g.ID)
	if err != nil || got.Name != "Home lab" || !slices.Equal(got.WorkerIDs, []string{"w-a", "w-c"}) || got.CreatedBy != "bsant" {
		t.Fatalf("group = %+v, %v", got, err)
	}
	got.Name, got.WorkerIDs = "Lab", []string{"w-b"}
	if err := st.UpdateMachineGroup(ctx, got); err != nil {
		t.Fatal(err)
	}
	groups, _ := st.ListMachineGroups(ctx)
	if len(groups) != 1 || groups[0].Name != "Lab" || !slices.Equal(groups[0].WorkerIDs, []string{"w-b"}) {
		t.Fatalf("groups after update = %+v", groups)
	}

	// Deleting a machine drops its labels and memberships.
	if err := st.DeleteWorker(ctx, "w-b"); err != nil {
		t.Fatal(err)
	}
	if labels, _ = st.ListMachineLabels(ctx); labels["w-b"] != nil {
		t.Fatalf("deleted machine kept labels %v", labels["w-b"])
	}
	if got, _ = st.GetMachineGroup(ctx, g.ID); len(got.WorkerIDs) != 0 {
		t.Fatalf("deleted machine still in group: %v", got.WorkerIDs)
	}
	if err := st.DeleteMachineGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMachineGroup(ctx, g.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted group: %v", err)
	}

	// Chat scope: all machines by default, stored, and cleared by setting all again.
	now := time.Now().UTC()
	if err := st.CreateChatSession(ctx, store.ChatSession{ID: "chat-1", Backend: store.ChatBackendAgent, FleetTools: true, Status: protocol.ChatStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if sc, err := st.GetChatScope(ctx, "chat-1"); err != nil || !sc.IsAll() {
		t.Fatalf("default scope = %+v, %v", sc, err)
	}
	want := store.MachineTarget{Kind: store.TargetLabel, Label: "Work"}
	if err := st.SetChatScope(ctx, "chat-1", want); err != nil {
		t.Fatal(err)
	}
	if sc, _ := st.GetChatScope(ctx, "chat-1"); sc.Kind != want.Kind || sc.Label != want.Label {
		t.Fatalf("scope = %+v, want %+v", sc, want)
	}
	if err := st.SetChatScope(ctx, "chat-nope", want); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("scope on an unknown chat: %v", err)
	}
	if err := st.SetChatScope(ctx, "chat-1", store.MachineTarget{Kind: store.TargetAll}); err != nil {
		t.Fatal(err)
	}
	if sc, _ := st.GetChatScope(ctx, "chat-1"); !sc.IsAll() {
		t.Fatalf("scope after reset = %+v", sc)
	}
}

func TestTicketScope(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tk, err := st.CreateTicket(ctx, store.Ticket{ProjectID: store.DefaultProjectID, Kind: store.TicketKindFeature, Title: "x", Status: store.TicketStatusReady})
	if err != nil {
		t.Fatal(err)
	}
	if sc, err := st.GetTicketScope(ctx, tk.ID); err != nil || !sc.IsAll() {
		t.Fatalf("default Run on = %+v, %v", sc, err)
	}
	want := store.MachineTarget{Kind: store.TargetGroup, ID: "g-1"}
	if err := st.SetTicketScope(ctx, tk.ID, want); err != nil {
		t.Fatal(err)
	}
	if sc, _ := st.GetTicketScope(ctx, tk.ID); sc.Kind != want.Kind || sc.ID != want.ID {
		t.Fatalf("Run on = %+v", sc)
	}
	if err := st.SetTicketScope(ctx, "tk-nope", want); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown ticket: %v", err)
	}
	if err := st.SetTicketScope(ctx, tk.ID, store.MachineTarget{Kind: store.TargetAll}); err != nil {
		t.Fatal(err)
	}
	if sc, _ := st.GetTicketScope(ctx, tk.ID); !sc.IsAll() {
		t.Fatalf("after reset = %+v", sc)
	}
}
