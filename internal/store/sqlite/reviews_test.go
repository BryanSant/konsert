package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

func TestReviewsSaveGetList(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if _, err := st.GetReview(ctx, "t1", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetReview(missing) err = %v, want ErrNotFound", err)
	}
	if _, err := st.SaveReview(ctx, store.Review{}); err == nil {
		t.Fatal("SaveReview without a session succeeded")
	}
	if _, err := st.SaveReview(ctx, store.Review{TaskID: "t1", ChatID: "c1"}); err == nil {
		t.Fatal("SaveReview with a task and a chat succeeded")
	}

	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	r, err := st.SaveReview(ctx, store.Review{TaskID: "t1", Expedite: true, ExpeditedAt: &at})
	if err != nil {
		t.Fatal(err)
	}
	if !ids.IsUUIDv7(r.ID) || !ids.Valid(ids.Review, r.SID) {
		t.Fatalf("ids %q / %q, want a UUIDv7 and an rv- sid", r.ID, r.SID)
	}
	// Updating keeps the identity and creation time.
	upd := r
	upd.Decision, upd.Note, upd.DecidedBy, upd.DecidedAt = store.ReviewApproved, "ship it", "admin", &at
	upd.DeliveryMode, upd.DeliveryStatus, upd.DeliveryMessage = store.DeliveryModePRMerge, store.DeliveryStatusAutoMerge, "Auto-merge enabled"
	upd.MergeMethod, upd.PRNumber, upd.PRURL, upd.PRState, upd.PRChecks, upd.PRAutoMerge = store.MergeStyleSquash, 12, "https://github.com/acme/wc/pull/12", "open", "pending", true
	upd.PRRemoteURL, upd.PRLastCommentAt = "https://github.com/acme/wc.git", &at
	upd.ID, upd.SID = "", "" // ignored: the session's row is updated
	got, err := st.SaveReview(ctx, upd)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != r.ID || got.SID != r.SID || !got.CreatedAt.Equal(r.CreatedAt) {
		t.Fatalf("update changed identity: %+v vs %+v", got, r)
	}
	back, err := st.GetReview(ctx, "t1", "")
	if err != nil {
		t.Fatal(err)
	}
	if back.Decision != store.ReviewApproved || back.Note != "ship it" || !back.Expedite || back.ExpeditedAt == nil ||
		!back.ExpeditedAt.Equal(at) || back.DecidedAt == nil || back.DeliveryStatus != store.DeliveryStatusAutoMerge ||
		back.PRNumber != 12 || back.PRURL == "" || !back.PRAutoMerge || back.PRLastCommentAt == nil {
		t.Fatalf("GetReview = %+v", back)
	}

	// Seen review comment deduplication is persisted.
	inserted, err := st.MarkReviewCommentSeen(ctx, "t1", "c-101", at)
	if err != nil || !inserted {
		t.Fatalf("first MarkReviewCommentSeen = (%v, %v), want (true, nil)", inserted, err)
	}
	inserted, err = st.MarkReviewCommentSeen(ctx, "t1", "c-101", at)
	if err != nil || inserted {
		t.Fatalf("duplicate MarkReviewCommentSeen = (%v, %v), want (false, nil)", inserted, err)
	}

	if _, err := st.SaveReview(ctx, store.Review{ChatID: "c1", Expedite: true}); err != nil {
		t.Fatal(err)
	}
	if c, err := st.GetReview(ctx, "", "c1"); err != nil || !c.Expedite || c.TaskID != "" {
		t.Fatalf("chat review %+v, %v", c, err)
	}
	all, err := st.ListReviews(ctx)
	if err != nil || len(all) != 2 || all[0].TaskID != "t1" || all[1].ChatID != "c1" {
		t.Fatalf("ListReviews = %+v, %v", all, err)
	}
	if id, err := st.ResolveID(ctx, ids.Review, r.SID); err != nil || id != r.ID {
		t.Fatalf("ResolveID(%s) = %q, %v", r.SID, id, err)
	}
}
