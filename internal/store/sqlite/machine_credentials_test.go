package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

func TestMachineCredentials(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

	if _, err := st.CreateMachineCredential(ctx, store.MachineCredential{WorkerID: "w1"}, ""); err == nil {
		t.Fatal("credential without a hash accepted")
	}
	a, err := st.CreateMachineCredential(ctx, store.MachineCredential{WorkerID: "w1", Source: store.MachineCredJoin, CreatedAt: at}, "hash-a")
	if err != nil {
		t.Fatal(err)
	}
	if !ids.IsUUIDv7(a.ID) || !ids.Valid(ids.MachineCredential, a.SID) {
		t.Fatalf("ids %q / %q, want a UUIDv7 and an mc- sid", a.ID, a.SID)
	}
	b, err := st.CreateMachineCredential(ctx, store.MachineCredential{WorkerID: "w1", Source: store.MachineCredRotate, CreatedAt: at.Add(time.Minute)}, "hash-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateMachineCredential(ctx, store.MachineCredential{WorkerID: "w2", CreatedAt: at}, "hash-c"); err != nil {
		t.Fatal(err)
	}

	got, hash, err := st.GetMachineCredential(ctx, a.ID)
	if err != nil || hash != "hash-a" || got.WorkerID != "w1" || got.Source != store.MachineCredJoin || !got.CreatedAt.Equal(at) {
		t.Fatalf("get = %+v %q %v", got, hash, err)
	}
	if _, _, err := st.GetMachineCredential(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing credential: %v", err)
	}
	if id, err := st.ResolveID(ctx, ids.MachineCredential, a.SID); err != nil || id != a.ID {
		t.Fatalf("resolve sid = %q %v", id, err)
	}

	if err := st.TouchMachineCredential(ctx, a.ID, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListMachineCredentials(ctx, "w1")
	if err != nil || len(list) != 2 || list[0].ID != b.ID || list[1].LastUsedAt == nil || !list[1].LastUsedAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("list w1 = %+v %v", list, err)
	}
	if all, _ := st.ListMachineCredentials(ctx, ""); len(all) != 3 {
		t.Fatalf("list all = %d, want 3", len(all))
	}

	// Revoke all but b, then b itself.
	n, err := st.RevokeMachineCredentials(ctx, "w1", b.ID, "rotated", at.Add(2*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("revoke except b = %d %v", n, err)
	}
	if got, _, _ := st.GetMachineCredential(ctx, a.ID); got.RevokedAt == nil || got.RevokedReason != "rotated" {
		t.Fatalf("a not revoked: %+v", got)
	}
	if n, _ := st.RevokeMachineCredentials(ctx, "w1", b.ID, "again", at); n != 0 {
		t.Fatalf("revoking twice revoked %d", n)
	}
	if err := st.RevokeMachineCredential(ctx, b.ID, "operator", at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.GetMachineCredential(ctx, b.ID); got.RevokedAt == nil || got.RevokedReason != "operator" {
		t.Fatalf("b not revoked: %+v", got)
	}
	// An already revoked credential keeps its first revocation.
	if err := st.RevokeMachineCredential(ctx, a.ID, "later", at.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.GetMachineCredential(ctx, a.ID); got.RevokedReason != "rotated" {
		t.Fatalf("revocation overwritten: %+v", got)
	}

	// Removing the machine deletes its credentials.
	if err := st.UpsertWorker(ctx, store.Worker{ID: "w2", Name: "w2", HostGroup: "w2", Status: "online", LastSeenAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteWorker(ctx, "w2"); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.ListMachineCredentials(ctx, "w2"); len(list) != 0 {
		t.Fatalf("credentials of a removed machine kept: %+v", list)
	}
}

func TestMachineJoinCodes(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

	c, err := st.CreateMachineJoinCode(ctx, store.MachineJoinCode{CreatedBy: "operator", CreatedAt: at, ExpiresAt: at.Add(15 * time.Minute)}, "code-1")
	if err != nil || !ids.IsUUIDv7(c.ID) {
		t.Fatalf("create = %+v %v", c, err)
	}
	if n, _ := st.CountActiveMachineJoinCodes(ctx, at); n != 1 {
		t.Fatalf("active = %d, want 1", n)
	}
	// Expired: not redeemable.
	if _, err := st.RedeemMachineJoinCode(ctx, "code-1", "w1", at.Add(16*time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired redeem: %v", err)
	}
	if _, err := st.RedeemMachineJoinCode(ctx, "wrong", "w1", at); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wrong code redeem: %v", err)
	}
	got, err := st.RedeemMachineJoinCode(ctx, "code-1", "w1", at.Add(time.Minute))
	if err != nil || got.ID != c.ID || got.WorkerID != "w1" || got.UsedAt == nil {
		t.Fatalf("redeem = %+v %v", got, err)
	}
	// Single use.
	if _, err := st.RedeemMachineJoinCode(ctx, "code-1", "w2", at.Add(2*time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second redeem: %v", err)
	}
	if n, _ := st.CountActiveMachineJoinCodes(ctx, at); n != 0 {
		t.Fatalf("active after use = %d", n)
	}
	if g, err := st.GetMachineJoinCode(ctx, c.ID); err != nil || g.WorkerID != "w1" {
		t.Fatalf("get = %+v %v", g, err)
	}

	// A new code prunes codes that expired over a day ago.
	later := at.Add(48 * time.Hour)
	if _, err := st.CreateMachineJoinCode(ctx, store.MachineJoinCode{CreatedAt: later, ExpiresAt: later.Add(15 * time.Minute)}, "code-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMachineJoinCode(ctx, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old code not pruned: %v", err)
	}
}
