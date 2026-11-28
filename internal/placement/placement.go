// Package placement scores the machines that passed the hard filters (requirements,
// policy, logins) for one piece of work (SYR-YCQQ1; docs/prd.md §5.5). The score
// weighs how well the work's expected peak fits the machine's headroom, checkout
// locality and how loaded the machine is; a machine cooling down after a shed counts
// for less. Work with no profile yet rotates across the machines, weighted by headroom,
// with a pick that is deterministic given its seed (the task), so the same inputs
// always choose the same machine.
package placement

import (
	"hash/fnv"
	"math"
	"sort"
	"time"

	"github.com/BryanSant/syre/internal/capacity"
)

// Weights of the score's parts (fit and locality dominate; spread breaks near-ties).
const (
	WeightFit      = 4.0
	WeightLocality = 3.0
	WeightSpread   = 1.0
)

// Work is what is known about the work being placed.
type Work struct {
	// PeakMB is its expected peak memory (0: unknown, the small class); Known says a
	// profile backs it.
	PeakMB uint64
	Known  bool
	// Seed makes the rotation of unknown work deterministic (a task or ticket id).
	Seed string
}

// Candidate is a machine that passed the hard filters.
type Candidate struct {
	ID string
	// Headroom is nil when unknown (no heartbeat yet).
	Headroom *capacity.Headroom
	// Active and Slots are its running sessions and slot limit.
	Active, Slots int
	// LocalityMatched of LocalityTotal bound repos have a checkout there (0/0: no repos).
	LocalityMatched, LocalityTotal int
	DiskFreeGB                     uint64
	LastSeen                       time.Time
	// RuleBonus is what placement rules add (prefer) or take away (avoid): enough to
	// beat any score, never the fit penalty of a machine that can't hold the work.
	RuleBonus float64
}

// Scored is a candidate with its score and the parts of it.
type Scored struct {
	Candidate
	Score    float64 `json:"score"`
	Fit      float64 `json:"fit"`      // -1 (doesn't fit) … 1 (plenty of room); 0 unknown
	Locality float64 `json:"locality"` // 0 … 1
	Spread   float64 `json:"spread"`   // 0 … 1, free slots
	Weight   float64 `json:"weight"`   // the headroom's weight (a shed down-weights)
	Rule     float64 `json:"rule,omitzero"`
	Fits     bool    `json:"fits"`
}

// RuleBonus is a prefer rule's bonus (an avoid rule's penalty): more than the whole
// score can be.
const RuleBonus = 2 * (WeightFit + WeightLocality + WeightSpread)

// fitScore is how comfortably peak fits headroom: 1 with twice the peak to spare or
// more, 0 just fitting; below 0 when it doesn't fit, toward -1 the less room there is.
func fitScore(h *capacity.Headroom, peak uint64) (float64, bool) {
	if h == nil {
		return 0, true
	}
	if peak == 0 {
		peak = capacity.SmallClassPeakMB
	}
	if !h.Fits(peak) {
		return -1 + math.Max(0, float64(h.RAMMB))/float64(peak), false
	}
	spare := float64(h.RAMMB-int(peak)) / float64(2*peak)
	return math.Min(1, spare), true
}

// Rank scores the candidates, best first. Ties fall back to free disk, the most
// recently seen, then the id.
func Rank(w Work, cands []Candidate) []Scored {
	out := make([]Scored, 0, len(cands))
	for _, c := range cands {
		s := Scored{Candidate: c, Weight: 1}
		s.Fit, s.Fits = fitScore(c.Headroom, w.PeakMB)
		if c.Headroom != nil {
			s.Weight = c.Headroom.Weight
		}
		if c.LocalityTotal > 0 {
			s.Locality = float64(c.LocalityMatched) / float64(c.LocalityTotal)
		}
		if c.Slots > 0 {
			s.Spread = math.Max(0, 1-float64(c.Active)/float64(c.Slots))
		}
		s.Score = (WeightFit*s.Fit+WeightLocality*s.Locality+WeightSpread*s.Spread)*s.Weight + c.RuleBonus
		s.Rule = c.RuleBonus
		if !s.Fits {
			s.Score -= 100 // last resort: only where nothing fits
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.DiskFreeGB != b.DiskFreeGB {
			return a.DiskFreeGB > b.DiskFreeGB
		}
		if !a.LastSeen.Equal(b.LastSeen) {
			return a.LastSeen.After(b.LastSeen)
		}
		return a.ID < b.ID
	})
	return out
}

// Choose picks the machine for w: the best ranked, or for unknown work a rotation
// among the candidates as good as the best on locality, weighted by their headroom.
// ok is false without candidates.
func Choose(w Work, cands []Candidate) (Scored, []Scored, bool) {
	ranked := Rank(w, cands)
	if len(ranked) == 0 {
		return Scored{}, nil, false
	}
	if w.Known || w.Seed == "" {
		return ranked[0], ranked, true
	}
	// Unknown work: rotate across the machines that fit and are as local as the best,
	// each in proportion to its headroom (times its weight).
	best := ranked[0]
	var pool []Scored
	var total float64
	for _, s := range ranked {
		if !s.Fits || s.Locality < best.Locality || s.Rule < best.Rule || s.Headroom == nil || s.Spread == 0 {
			continue
		}
		if wt := rotationWeight(s); wt > 0 {
			pool = append(pool, s)
			total += wt
		}
	}
	if len(pool) < 2 {
		return best, ranked, true
	}
	x := seedFraction(w.Seed) * total
	for _, s := range pool {
		x -= rotationWeight(s)
		if x < 0 {
			return s, ranked, true
		}
	}
	return pool[len(pool)-1], ranked, true
}

func rotationWeight(s Scored) float64 {
	return math.Max(0, float64(s.Headroom.RAMMB)) * s.Weight
}

// seedFraction maps a seed to [0, 1) deterministically. FNV-1a's high bits barely
// move for seeds that differ only at the end ("task-1", "task-2"), so a splitmix64
// finalizer mixes them first.
func seedFraction(seed string) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	x := h.Sum64()
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return float64(x>>11) / float64(1<<53)
}
