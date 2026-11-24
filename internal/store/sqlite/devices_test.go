package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/store"
)

func TestDevicesLifecycle(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

	if _, err := st.CreateDevice(ctx, store.Device{Name: "x", Kind: "toaster", SecretHash: "h"}); err == nil {
		t.Fatal("invalid kind accepted")
	}
	if _, err := st.CreateDevice(ctx, store.Device{Name: "x", Kind: store.DeviceKindCLI}); err == nil {
		t.Fatal("device without a secret hash accepted")
	}
	d, err := st.CreateDevice(ctx, store.Device{Name: "Laptop", Kind: store.DeviceKindBrowser, SecretHash: "hash-1",
		CreatedAt: t0, ExpiresAt: t0.Add(90 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if !ids.IsUUIDv7(d.ID) || !ids.Valid(ids.Device, d.SID) {
		t.Fatalf("ids: %q %q", d.ID, d.SID)
	}
	if id, err := st.ResolveID(ctx, ids.Device, d.SID); err != nil || id != d.ID {
		t.Fatalf("ResolveID(sid) = %q, %v", id, err)
	}
	got, err := st.GetDeviceBySecretHash(ctx, "hash-1")
	if err != nil || got.ID != d.ID || !got.LastUsedAt.Equal(t0) || got.RevokedAt != nil {
		t.Fatalf("GetDeviceBySecretHash = %+v, %v", got, err)
	}
	if _, err := st.GetDeviceBySecretHash(ctx, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty hash: %v", err)
	}

	t1 := t0.Add(time.Hour)
	if err := st.TouchDevice(ctx, d.ID, t1, t1.Add(time.Hour), "10.0.0.2", "Chrome on macOS"); err != nil {
		t.Fatal(err)
	}
	if err := st.RenameDevice(ctx, d.ID, "Work laptop"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetDevice(ctx, d.ID)
	if got.Name != "Work laptop" || !got.LastUsedAt.Equal(t1) || got.LastIP != "10.0.0.2" || got.LastUserAgent != "Chrome on macOS" {
		t.Fatalf("after touch/rename: %+v", got)
	}
	d2, _ := st.CreateDevice(ctx, store.Device{Name: "Phone", Kind: store.DeviceKindPhone, SecretHash: "hash-2", CreatedAt: t0.Add(2 * time.Hour)})
	list, err := st.ListDevices(ctx)
	if err != nil || len(list) != 2 || list[0].ID != d2.ID {
		t.Fatalf("ListDevices = %+v, %v", list, err)
	}

	if ok, err := st.RevokeDevice(ctx, d.ID, t1); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if ok, _ := st.RevokeDevice(ctx, d.ID, t1); ok {
		t.Fatal("second revoke reported success")
	}
	if err := st.RenameDevice(ctx, d.ID, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rename revoked: %v", err)
	}
	got, _ = st.GetDeviceBySecretHash(ctx, "hash-1")
	if got.RevokedAt == nil {
		t.Fatal("revoked device has no revoked_at")
	}
	if list, _ := st.ListDevices(ctx); len(list) != 1 {
		t.Fatalf("revoked device still listed: %+v", list)
	}
	if err := st.PruneAuth(ctx, t1.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDevice(ctx, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pruned revoked device: %v", err)
	}
	if _, err := st.GetDevice(ctx, d2.ID); err != nil {
		t.Fatalf("active device pruned: %v", err)
	}
}

func TestPairingCodeRedeemOnce(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	p, err := st.CreatePairingCode(ctx, store.PairingCode{Purpose: store.PairingPurposePair, CodeHash: "c1",
		CreatedAt: t0, ExpiresAt: t0.Add(10 * time.Minute)})
	if err != nil || !ids.Valid(ids.PairingCode, p.SID) {
		t.Fatalf("create: %+v %v", p, err)
	}
	if got, err := st.GetPairingCodeByHash(ctx, "c1"); err != nil || got.ID != p.ID {
		t.Fatalf("by hash: %+v %v", got, err)
	}

	// Concurrent redemptions: exactly one wins and exactly one device exists.
	var wg sync.WaitGroup
	wins := make(chan store.Device, 8)
	for i := range 8 {
		wg.Go(func() {
			d, ok, err := st.RedeemPairingCode(ctx, p.ID, store.Device{Name: "B", Kind: store.DeviceKindBrowser,
				SecretHash: "s" + string(rune('a'+i)), CreatedAt: t0.Add(time.Minute), ExpiresAt: t0.Add(time.Hour)}, t0.Add(time.Minute))
			if err != nil {
				t.Error(err)
			}
			if ok {
				wins <- d
			}
		})
	}
	wg.Wait()
	close(wins)
	var won []store.Device
	for d := range wins {
		won = append(won, d)
	}
	if len(won) != 1 {
		t.Fatalf("%d redemptions won, want 1", len(won))
	}
	if list, _ := st.ListDevices(ctx); len(list) != 1 || list[0].ID != won[0].ID {
		t.Fatalf("devices after redemption: %+v", list)
	}
	got, _ := st.GetPairingCodeByHash(ctx, "c1")
	if got.RedeemedAt == nil || got.DeviceID != won[0].ID {
		t.Fatalf("code after redemption: %+v", got)
	}

	// An expired code never redeems.
	p2, _ := st.CreatePairingCode(ctx, store.PairingCode{Purpose: store.PairingPurposePair, CodeHash: "c2", CreatedAt: t0, ExpiresAt: t0.Add(time.Minute)})
	if _, ok, _ := st.RedeemPairingCode(ctx, p2.ID, store.Device{Name: "B", Kind: store.DeviceKindBrowser, SecretHash: "late"}, t0.Add(2*time.Minute)); ok {
		t.Fatal("expired code redeemed")
	}
	if err := st.PruneAuth(ctx, t0.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPairingCodeByHash(ctx, "c2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired code not pruned: %v", err)
	}
}

func TestLoginCodeNeedsApproval(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	p, err := st.CreatePairingCode(ctx, store.PairingCode{Purpose: store.PairingPurposeLogin, CodeHash: "c1", PollHash: "p1",
		Kind: store.DeviceKindCLI, DeviceName: "build box", CreatedAt: t0, ExpiresAt: t0.Add(10 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetPairingCodeByPollHash(ctx, "p1"); err != nil || got.ID != p.ID || got.DeviceName != "build box" {
		t.Fatalf("by poll hash: %+v %v", got, err)
	}
	dev := store.Device{Name: "build box", Kind: store.DeviceKindCLI, SecretHash: "s1"}
	if _, ok, _ := st.RedeemPairingCode(ctx, p.ID, dev, t0.Add(time.Minute)); ok {
		t.Fatal("unapproved login code redeemed")
	}
	if ok, err := st.DecidePairingCode(ctx, p.ID, true, "Laptop (dv-x)", t0.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("approve: %v %v", ok, err)
	}
	if ok, _ := st.DecidePairingCode(ctx, p.ID, false, "x", t0.Add(time.Minute)); ok {
		t.Fatal("decided twice")
	}
	if _, ok, err := st.RedeemPairingCode(ctx, p.ID, dev, t0.Add(2*time.Minute)); err != nil || !ok {
		t.Fatalf("approved login code: %v %v", ok, err)
	}

	// A denied code never redeems; a pair code can't be "decided".
	p2, _ := st.CreatePairingCode(ctx, store.PairingCode{Purpose: store.PairingPurposeLogin, CodeHash: "c2", PollHash: "p2", Kind: store.DeviceKindCLI, CreatedAt: t0, ExpiresAt: t0.Add(10 * time.Minute)})
	if ok, _ := st.DecidePairingCode(ctx, p2.ID, false, "x", t0.Add(time.Minute)); !ok {
		t.Fatal("deny failed")
	}
	if _, ok, _ := st.RedeemPairingCode(ctx, p2.ID, store.Device{Name: "y", Kind: store.DeviceKindCLI, SecretHash: "s2"}, t0.Add(2*time.Minute)); ok {
		t.Fatal("denied code redeemed")
	}
	p3, _ := st.CreatePairingCode(ctx, store.PairingCode{Purpose: store.PairingPurposePair, CodeHash: "c3", CreatedAt: t0, ExpiresAt: t0.Add(10 * time.Minute)})
	if ok, _ := st.DecidePairingCode(ctx, p3.ID, true, "x", t0.Add(time.Minute)); ok {
		t.Fatal("pair code approved")
	}
}
