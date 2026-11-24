package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/store"
)

func TestOverrideLifecycle(t *testing.T) {
	ctx := context.Background()
	st := openControlStore(t)
	o, err := st.CreateOverride(ctx, store.Override{ScopeKind: store.OverrideScopeMachineSlots, WorkerID: "w1", ExtraSlots: 1, DurationSeconds: 1800, Reason: "release"})
	if err != nil {
		t.Fatal(err)
	}
	if !ids.IsUUIDv7(o.ID) || !ids.Valid(ids.Override, o.SID) || o.Status != store.OverridePending {
		t.Fatalf("created: %+v", o)
	}
	if id, err := st.ResolveID(ctx, ids.Override, o.SID); err != nil || id != o.ID {
		t.Fatalf("resolve sid: %q %v", id, err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	exp := now.Add(30 * time.Minute)
	if ok, err := st.DecideOverride(ctx, o.ID, store.OverrideApproved, "tester", "ok", now, &exp); err != nil || !ok {
		t.Fatalf("approve: %v %v", ok, err)
	}
	if ok, _ := st.DecideOverride(ctx, o.ID, store.OverrideDenied, "tester", "", now, nil); ok {
		t.Fatal("decided twice")
	}
	got, _ := st.GetOverride(ctx, o.ID)
	if got.Status != store.OverrideApproved || got.ApprovedAt == nil || got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) ||
		!got.ActiveAt(now.Add(time.Minute)) || got.ActiveAt(exp) || got.Used() {
		t.Fatalf("approved: %+v", got)
	}
	for range 2 {
		if err := st.MarkOverrideUsed(ctx, o.ID, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := st.ExpireOverride(ctx, o.ID, exp); !ok {
		t.Fatal("expire")
	}
	got, _ = st.GetOverride(ctx, o.ID)
	if got.Status != store.OverrideExpired || got.UseCount != 2 || got.FirstUsedAt == nil || got.EndedAt == nil {
		t.Fatalf("expired: %+v", got)
	}
	if list, _ := st.ListOverrides(ctx, store.OverrideFilter{Status: store.OverrideApproved}); len(list) != 0 {
		t.Fatalf("approved list after expiry: %+v", list)
	}
}

func TestVetoDeliveryAckEscalateDismiss(t *testing.T) {
	ctx := context.Background()
	st := openControlStore(t)
	a, _ := st.CreateVeto(ctx, store.Veto{Reason: "not_now", Summary: "a", FleetEventID: 10})
	b, _ := st.CreateVeto(ctx, store.Veto{Reason: "dangerous", Summary: "b", FleetEventID: 11})
	if !ids.Valid(ids.Veto, a.SID) {
		t.Fatalf("sid %q", a.SID)
	}
	now := time.Now().UTC()
	if vs, err := st.MarkVetoesDelivered(ctx, []int64{10, 11}, "c1", "turn-1", now); err != nil || len(vs) != 2 {
		t.Fatalf("delivered: %+v %v", vs, err)
	}
	// Delivered again (another chat's turn): the latest delivery is the one that counts.
	if _, err := st.MarkVetoesDelivered(ctx, []int64{11}, "c2", "turn-9", now); err != nil {
		t.Fatal(err)
	}
	if acked, _ := st.AckVetoes(ctx, "c1", "other-turn", now); len(acked) != 0 {
		t.Fatalf("acked by another turn: %+v", acked)
	}
	acked, err := st.AckVetoes(ctx, "c1", "turn-1", now)
	if err != nil || len(acked) != 1 || acked[0].ID != a.ID || acked[0].AckedChatID != "c1" {
		t.Fatalf("ack: %+v %v", acked, err)
	}
	open, _ := st.ListVetoes(ctx, store.VetoFilter{OpenOnly: true})
	if len(open) != 1 || open[0].ID != b.ID {
		t.Fatalf("open: %+v", open)
	}
	if ok, _ := st.EscalateVeto(ctx, b.ID, now); !ok {
		t.Fatal("escalate")
	}
	if ok, _ := st.EscalateVeto(ctx, b.ID, now); ok {
		t.Fatal("escalated twice")
	}
	if ok, _ := st.DismissVeto(ctx, a.ID, "tester", now); ok {
		t.Fatal("dismissed an acknowledged veto")
	}
	if ok, _ := st.DismissVeto(ctx, b.ID, "tester", now); !ok {
		t.Fatal("dismiss")
	}
	if open, _ := st.ListVetoes(ctx, store.VetoFilter{OpenOnly: true}); len(open) != 0 {
		t.Fatalf("open after dismiss: %+v", open)
	}
}
