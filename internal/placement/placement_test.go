package placement

import (
	"fmt"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/capacity"
)

func hr(ramMB int, weight float64) *capacity.Headroom {
	return &capacity.Headroom{RAMMB: ramMB, Weight: weight}
}

// A synthetic fleet: a big build box, a small laptop, a box cooling down after a shed,
// one full of sessions, and one with the repo checked out.
func fleet() []Candidate {
	now := time.Unix(1_800_000_000, 0)
	return []Candidate{
		{ID: "big", Headroom: hr(40000, 1), Active: 1, Slots: 8, LastSeen: now},
		{ID: "laptop", Headroom: hr(3000, 1), Active: 0, Slots: 2, LastSeen: now},
		{ID: "shed", Headroom: hr(40000, capacity.ShedWeight), Active: 0, Slots: 8, LastSeen: now},
		{ID: "busy", Headroom: hr(20000, 1), Active: 3, Slots: 4, LastSeen: now},
		{ID: "local", Headroom: hr(12000, 1), Active: 1, Slots: 4, LocalityMatched: 1, LocalityTotal: 1, LastSeen: now},
	}
}

func TestKnownBigWorkGoesWhereItFits(t *testing.T) {
	cands := fleet()
	for i := range cands {
		cands[i].LocalityTotal = 1 // one repo bound; only "local" has it
	}
	// 16 GB peak: only big and shed fit; big wins (shed is down-weighted), local can't hold it.
	best, ranked, ok := Choose(Work{PeakMB: 16000, Known: true}, cands)
	if !ok || best.ID != "big" {
		t.Fatalf("best %s, ranked %v", best.ID, ids(ranked))
	}
	for _, s := range ranked {
		if (s.ID == "laptop" || s.ID == "local") && s.Fits {
			t.Fatalf("%s fits 16 GB: %+v", s.ID, s)
		}
	}
	if ranked[len(ranked)-1].Fits {
		t.Fatalf("a machine that doesn't fit isn't last: %v", ids(ranked))
	}
}

func TestKnownSmallWorkPrefersLocality(t *testing.T) {
	cands := fleet()
	for i := range cands {
		cands[i].LocalityTotal = 1
	}
	best, ranked, _ := Choose(Work{PeakMB: 2000, Known: true}, cands)
	if best.ID != "local" {
		t.Fatalf("best %s, ranked %v", best.ID, ids(ranked))
	}
}

func TestUnknownWorkRotatesWeightedByHeadroom(t *testing.T) {
	cands := fleet()
	cands[4].LocalityMatched, cands[4].LocalityTotal = 0, 0 // no repo bound
	counts := map[string]int{}
	for i := range 2000 {
		best, _, _ := Choose(Work{Seed: fmt.Sprintf("task-%d", i)}, cands)
		counts[best.ID]++
	}
	// No locality bound: big (40 GB), local (12 GB), busy (20 GB), laptop (3 GB) and
	// shed (40 GB × 0.25) share the work in proportion; big gets the most, laptop the least.
	if counts["big"] <= counts["busy"] || counts["busy"] <= counts["local"] || counts["local"] <= counts["laptop"] || counts["laptop"] == 0 {
		t.Fatalf("rotation %v", counts)
	}
	share := float64(counts["big"]) / 2000
	if share < 0.40 || share > 0.55 { // 40 / (40+3+10+20+12) ≈ 0.47
		t.Fatalf("big's share %.2f (%v)", share, counts)
	}
}

func TestDeterministicGivenInputs(t *testing.T) {
	cands := fleet()
	for _, w := range []Work{{Seed: "task-7"}, {PeakMB: 8000, Known: true}, {Seed: "tk-abc", PeakMB: 500}} {
		first, firstRanked, _ := Choose(w, cands)
		for range 20 {
			again, ranked, _ := Choose(w, cands)
			if again.ID != first.ID || fmt.Sprint(ids(ranked)) != fmt.Sprint(ids(firstRanked)) {
				t.Fatalf("%+v: %s then %s", w, first.ID, again.ID)
			}
		}
	}
}

func TestNothingFitsStillPlacesOnTheRoomiest(t *testing.T) {
	cands := []Candidate{{ID: "a", Headroom: hr(1000, 1), Slots: 2}, {ID: "b", Headroom: hr(3000, 1), Slots: 2}}
	best, _, _ := Choose(Work{PeakMB: 64000, Known: true}, cands)
	if best.ID != "b" || best.Fits {
		t.Fatalf("best %+v", best)
	}
	// Unknown headroom (no heartbeat yet) neither helps nor hurts.
	best, _, _ = Choose(Work{PeakMB: 2000, Known: true}, []Candidate{{ID: "x", Slots: 2}, {ID: "y", Slots: 2, Active: 1}})
	if best.ID != "x" {
		t.Fatalf("spread: %+v", best)
	}
}

func ids(s []Scored) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.ID
	}
	return out
}
