package controller

import (
	"context"
	"testing"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestFollowupsFiledToBacklogUnlessExpandScope(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()

	run := func(name string, expand *bool) (epic store.Ticket, filed []store.Ticket) {
		t.Helper()
		if err := cs.st.CreateProject(ctx, store.Project{Name: name, Autonomy: store.AutonomyPolicy{ExpandScope: expand}}); err != nil {
			t.Fatal(err)
		}
		projs, _ := cs.st.ListProjects(ctx)
		proj := projs[len(projs)-1]
		epic, err := cs.st.CreateTicket(ctx, store.Ticket{ProjectID: proj.ID, Kind: store.TicketKindEpic, Title: "Epic " + name, Status: store.TicketStatusReady})
		if err != nil {
			t.Fatal(err)
		}
		tk, err := cs.st.CreateTicket(ctx, store.Ticket{ProjectID: proj.ID, ParentID: epic.ID, Kind: store.TicketKindFeature, Title: "Work " + name, Status: store.TicketStatusReview})
		if err != nil {
			t.Fatal(err)
		}
		task := &store.Task{Followups: []store.TaskFollowup{{Title: "Legacy endpoint still static", Why: "v1 token", Scope: "api"}}}
		dig := &protocol.RunDigest{TicketSID: tk.SID, Status: protocol.DigestStatusReview, Followups: []string{"Legacy endpoint still static — v1 token [api]"}}
		cs.h.pulseTicketTransition(ctx, tk, task, dig, "")
		cs.h.pulseTicketTransition(ctx, tk, task, dig, "") // a second report must not duplicate
		list, _ := cs.st.ListTickets(ctx, store.TicketFilter{ProjectID: proj.ID, ParentID: epic.ID})
		for _, c := range list {
			if c.ID != tk.ID {
				filed = append(filed, c)
			}
		}
		return epic, filed
	}

	_, filed := run("off", nil)
	if len(filed) != 1 {
		t.Fatalf("expand_scope off: filed %d followups, want 1: %+v", len(filed), filed)
	}
	f := filed[0]
	if f.Status != store.TicketStatusBacklog || f.Title != "Legacy endpoint still static" || len(f.Labels) != 1 || f.Labels[0] != "followup" {
		t.Fatalf("filed followup = %+v", f)
	}

	on := true
	if _, filed := run("on", &on); len(filed) != 0 {
		t.Fatalf("expand_scope on: Syre filed %d followups itself; the Commander triages them", len(filed))
	}
}
