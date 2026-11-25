package store

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// Idempotent ingestion of a daemon's spooled records (SYR-VZR9F, docs/design/spool.md).
//
// The hub applies each (worker, daemon seq) at most once. It checks the seq against the
// worker's applied seq before applying a record; writes that append (chat events, task
// events, usage, vitals) also advance the stored applied seq in their own transaction,
// guarded by it, so a replay after a hub crash cannot apply them twice.

// ErrSpoolDuplicate is returned by a guarded write whose record was already applied.
var ErrSpoolDuplicate = errors.New("spooled record already applied")

// SpoolMark identifies the spooled record a write applies.
type SpoolMark struct {
	WorkerID string
	Seq      uint64
	// claimed is set by the first guarded write of the record: later writes of the same
	// record (one message can append more than one row) are not duplicates.
	claimed atomic.Bool
}

// Claim reports whether this is the first guarded write of the record and marks it.
func (m *SpoolMark) Claim() bool { return m.claimed.CompareAndSwap(false, true) }

// Claimed reports whether a guarded write already applied the record.
func (m *SpoolMark) Claimed() bool { return m.claimed.Load() }

type spoolMarkKey struct{}

// WithSpoolMark returns ctx carrying the record that writes under it apply.
func WithSpoolMark(ctx context.Context, m *SpoolMark) context.Context {
	return context.WithValue(ctx, spoolMarkKey{}, m)
}

// SpoolMarkFrom returns the record mark carried by ctx (nil when none).
func SpoolMarkFrom(ctx context.Context) *SpoolMark {
	m, _ := ctx.Value(spoolMarkKey{}).(*SpoolMark)
	return m
}

// SpoolState is a worker's spool as the hub applied it.
type SpoolState struct {
	WorkerID   string
	Epoch      string
	AppliedSeq uint64
	UpdatedAt  time.Time
}

// SpoolStore persists applied spool seqs.
type SpoolStore interface {
	// GetSpoolState returns workerID's state (zero AppliedSeq and empty Epoch when none).
	GetSpoolState(ctx context.Context, workerID string) (SpoolState, error)
	// ResetSpoolEpoch starts epoch for workerID with nothing applied.
	ResetSpoolEpoch(ctx context.Context, workerID, epoch string) error
	// AdvanceSpoolApplied raises workerID's applied seq to seq (never lowers it).
	AdvanceSpoolApplied(ctx context.Context, workerID string, seq uint64) error
	// StoreLateVitals stores vitals sampled in the past (a daemon's replay) at their
	// original time: raw samples still in the raw tier's window go there, older ones are
	// merged straight into the hourly (or, past its window, daily) roll-ups and the
	// affected complete days are recomputed. Guarded by the ctx's spool mark.
	StoreLateVitals(ctx context.Context, samples []VitalsSample, now time.Time) error
}
