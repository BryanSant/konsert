package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

func TestDirectiveCreateListEnd(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	until := now.Add(30 * time.Minute)
	maxN := 1
	created, err := st.CreateDirective(ctx, store.Directive{
		Kind:       store.DirectiveMachineThrottle,
		Target:     "w-1",
		Params:     store.DirectiveParams{MaxConcurrency: &maxN},
		Until:      &until,
		Provenance: store.ProvenanceUser,
		Actor:      "local",
		Reason:     "lunch",
		CreatedAt:  now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ids.Valid(ids.Directive, created.SID) {
		t.Fatalf("sid %q", created.SID)
	}
	if created.Params.MaxConcurrency == nil || *created.Params.MaxConcurrency != 1 {
		t.Fatalf("params: %+v", created.Params)
	}
	got, err := st.GetDirective(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ActiveAt(now) || got.Until == nil || !got.Until.Equal(until) {
		t.Fatalf("active row: %+v", got)
	}

	active, err := st.ListDirectives(ctx, store.DirectiveFilter{ActiveOnly: true, Now: now, Target: "w-1"})
	if err != nil || len(active) != 1 {
		t.Fatalf("active: %v %+v", err, active)
	}
	later := until.Add(time.Second)
	active, err = st.ListDirectives(ctx, store.DirectiveFilter{ActiveOnly: true, Now: later, Target: "w-1"})
	if err != nil || len(active) != 0 {
		t.Fatalf("past until should be inactive: %v %+v", err, active)
	}
	open, err := st.ListDirectives(ctx, store.DirectiveFilter{OpenOnly: true, Target: "w-1"})
	if err != nil || len(open) != 1 {
		t.Fatalf("open past until: %v %+v", err, open)
	}

	ok, err := st.EndDirective(ctx, created.ID, "local", later)
	if err != nil || !ok {
		t.Fatalf("end: %v %v", ok, err)
	}
	ok, err = st.EndDirective(ctx, created.ID, "again", later)
	if err != nil || ok {
		t.Fatalf("second end should be a no-op: %v %v", ok, err)
	}
	ended, err := st.GetDirective(ctx, created.ID)
	if err != nil || ended.EndedAt == nil || ended.EndedBy != "local" || ended.ActiveAt(now) {
		t.Fatalf("ended: %+v %v", ended, err)
	}
}
