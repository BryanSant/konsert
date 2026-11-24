package controller

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

func TestRetentionSettingsBoundsAndText(t *testing.T) {
	r := retentionFromSettings(map[string]string{
		settingRetentionDetail: "45", settingRetentionFleet: "9999", settingRetentionUsage: "x",
	})
	if r.DetailDays != 45 || r.FleetDays != 90 || r.UsageDays != 30 || r.FleetSummaryDays != 365 {
		t.Fatalf("parsed %+v", r)
	}
	if err := r.validate(); err != nil {
		t.Fatal(err)
	}
	bad := r
	bad.DetailDays = 1
	if bad.validate() == nil {
		t.Fatal("1-day detail accepted")
	}
	bad = r
	bad.FleetSummaryDays = 60
	if bad.validate() == nil {
		t.Fatal("summary shorter than full timeline accepted")
	}
	if text := strings.Join(r.RetentionText(), " "); !strings.Contains(text, "45 days") || !strings.Contains(text, "Keep") {
		t.Fatalf("text %q", text)
	}
}

// TestCompactionPassAndStorageAPI drives Compact now and the Keep toggle over REST on a
// hub with an injected clock: a pass compacts old finished work in bounded batches,
// leaves pinned work alone, and is audit-logged.
func TestCompactionPassAndStorageAPI(t *testing.T) {
	srv, ts := newSecurityTestServer(t, nil)
	ctx := context.Background()
	st := srv.store
	h := srv.Hub()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	h.retentionNow = func() time.Time { return now }
	h.RetentionBatch = 7
	h.RetentionPause = 0

	old := now.Add(-40 * 24 * time.Hour)
	mkChat := func() string {
		cs := store.ChatSession{ID: ids.UUID(), Title: "old chat", Provider: "claude", Model: "m", Status: protocol.ChatStatusIdle, CreatedAt: old}
		if err := st.CreateChatSession(ctx, cs); err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 30; i++ {
			kind := protocol.EventToolCall
			if i%10 == 0 {
				kind = protocol.EventMessage
			}
			if _, err := st.AppendChatEvent(ctx, store.ChatEvent{ChatID: cs.ID, Seq: int64(i), Kind: kind, JSON: fmt.Sprintf(`{"kind":%q,"seq":%d}`, kind, i), CreatedAt: old}); err != nil {
				t.Fatal(err)
			}
		}
		return cs.ID
	}
	plain, pinned := mkChat(), mkChat()

	jsonHdr := map[string]string{"Content-Type": "application/json", "X-Konsert-Request": "1"}
	resp := doReq(t, http.MethodPut, ts.URL+"/api/chats/"+pinned+"/keep", strings.NewReader(`{"keep":true}`), jsonHdr)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("keep: %d %s", resp.StatusCode, b)
	}
	if cs, _ := st.GetChatSession(ctx, pinned); !cs.Keep {
		t.Fatal("chat not pinned")
	}

	resp = doReq(t, http.MethodPost, ts.URL+"/api/storage/compact", nil, jsonHdr)
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("compact: %d %s", resp.StatusCode, b)
	}
	waitFor(t, 10*time.Second, "compaction to finish", func() bool {
		run := h.CompactionState()
		return !run.Running && run.FinishedAt != nil
	})
	run := h.CompactionState()
	if run.Error != "" || !run.Complete || run.Rows != 27 || run.Trigger != "manual" {
		t.Fatalf("run %+v", run)
	}
	// 27 rows in batches of at most 7: at least 4 batches for the chat step.
	if run.Batches < 4+len(store.CompactSteps) {
		t.Fatalf("batches %d", run.Batches)
	}
	if evs, _ := st.ListChatEvents(ctx, plain, 0, 100); len(evs) != 3 {
		t.Fatalf("plain chat kept %d events, want 3 messages", len(evs))
	}
	if evs, _ := st.ListChatEvents(ctx, pinned, 0, 100); len(evs) != 30 {
		t.Fatalf("pinned chat kept %d events, want 30", len(evs))
	}
	if cs, _ := st.GetChatSession(ctx, plain); cs.CompactedAt == nil || !cs.CompactedAt.Equal(now) {
		t.Fatalf("plain chat compacted_at = %v", cs.CompactedAt)
	}
	audit, _ := st.ListAudit(ctx, store.AuditFilter{Action: AuditStorageCompact})
	if len(audit) != 1 {
		t.Fatalf("compact audit events = %d", len(audit))
	}

	resp = doReq(t, http.MethodGet, ts.URL+"/api/storage?fresh=1", nil, nil)
	body, _ := io.ReadAll(resp.Body)
	var sr StorageResponse
	if err := json.Unmarshal(body, &sr); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("storage: %d %s %v", resp.StatusCode, body, err)
	}
	if sr.Stats.FileBytes <= 0 || len(sr.Stats.Breakdown) != 7 || sr.Retention.DetailDays != 30 || len(sr.RetentionText) == 0 || sr.Compaction.Rows != 27 {
		t.Fatalf("storage response %+v", sr)
	}

	resp = doReq(t, http.MethodPut, ts.URL+"/api/storage/retention", strings.NewReader(`{"detail_days":3,"fleet_days":90,"fleet_summary_days":365,"usage_days":30,"usage_daily_days":365}`), jsonHdr)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("out-of-bounds retention: %d", resp.StatusCode)
	}
	resp = doReq(t, http.MethodPut, ts.URL+"/api/storage/retention", strings.NewReader(`{"detail_days":60,"fleet_days":90,"fleet_summary_days":365,"usage_days":30,"usage_daily_days":365}`), jsonHdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retention: %d", resp.StatusCode)
	}
	kv, _ := st.GetAllSettings(ctx)
	if retentionFromSettings(kv).DetailDays != 60 {
		t.Fatalf("settings %v", kv)
	}
}

// TestPeriodicPassIsBounded checks a scheduled pass stops at its batch budget and the
// next pass carries on.
func TestPeriodicPassIsBounded(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	th.h.retentionNow = func() time.Time { return now }
	th.h.RetentionBatch = 1
	th.h.RetentionPause = 0
	for i := range retentionTickBatches + 10 {
		if err := th.st.RecordUsage(ctx, store.UsageEvent{Provider: "p", Model: "m", InputTokens: 1, CreatedAt: now.Add(-40*24*time.Hour - time.Duration(i)*time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	run, err := th.h.runCompaction(ctx, "schedule", false)
	if err != nil {
		t.Fatal(err)
	}
	if run.Complete || run.Batches != retentionTickBatches || run.LongestBatchMS <= 0 {
		t.Fatalf("first pass %+v", run)
	}
	run, err = th.h.runCompaction(ctx, "schedule", false)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Complete {
		t.Fatalf("second pass %+v", run)
	}
	sum, _ := th.st.GetUsageSummary(ctx)
	if sum.TotalInputTokens != int64(retentionTickBatches+10) {
		t.Fatalf("usage total %d", sum.TotalInputTokens)
	}
}
