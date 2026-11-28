package store

import (
	"sort"
	"time"
)

// WorkProfile is what Syre has learned about one kind of work (a work key) on one
// machine class from its recent attempts (SYR-VYFYX; docs/prd.md §5.4).
type WorkProfile struct {
	WorkKey      string `json:"work_key"`
	MachineClass string `json:"machine_class"`
	// Runs counts the ended attempts considered (canceled ones are not outcomes);
	// SuccessRate is Successes / Runs.
	Runs        int     `json:"runs"`
	Successes   int     `json:"successes"`
	Failures    int     `json:"failures"`
	Canceled    int     `json:"canceled,omitzero"`
	SuccessRate float64 `json:"success_rate"`
	// Durations of successful attempts, in seconds.
	DurationAvgS float64 `json:"duration_avg_s,omitzero"`
	DurationP50S float64 `json:"duration_p50_s,omitzero"`
	DurationP90S float64 `json:"duration_p90_s,omitzero"`
	// Peak memory and CPU-seconds of attempts with metric history.
	PeakRSSAvgMB  float64 `json:"peak_rss_avg_mb,omitzero"`
	PeakRSSMaxMB  uint64  `json:"peak_rss_max_mb,omitzero"`
	CPUSecondsAvg float64 `json:"cpu_s_avg,omitzero"`
	// CommonFailures are the most frequent failure categories, most frequent first.
	CommonFailures []FailureCount `json:"common_failures,omitempty"`
	LastAttemptAt  *time.Time     `json:"last_attempt_at,omitzero"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// FailureCount is one failure category and how many attempts ended with it.
type FailureCount struct {
	Category string `json:"category"`
	N        int    `json:"n"`
}

// ProfileWindow is how many recent attempts a profile is computed from: recent enough
// to follow changes in the work, enough to smooth one bad run.
const ProfileWindow = 50

// ComputeWorkProfile summarises ended attempts (any order) into a profile.
func ComputeWorkProfile(workKey, class string, attempts []Attempt, now time.Time) WorkProfile {
	p := WorkProfile{WorkKey: workKey, MachineClass: class, UpdatedAt: now.UTC()}
	var durs []float64
	var peakSum, cpuSum float64
	var peakN, cpuN int
	fails := map[string]int{}
	for _, a := range attempts {
		if a.EndedAt == nil {
			continue
		}
		if p.LastAttemptAt == nil || a.EndedAt.After(*p.LastAttemptAt) {
			t := *a.EndedAt
			p.LastAttemptAt = &t
		}
		switch a.Status {
		case AttemptCanceled:
			p.Canceled++
			continue
		case AttemptCompleted, AttemptReview:
			p.Successes++
			if d := attemptDuration(a); d > 0 {
				durs = append(durs, d)
			}
		default: // failed, needs_you
			p.Failures++
			if a.FailureCategory != "" {
				fails[a.FailureCategory]++
			}
		}
		p.Runs++
		if a.PeakRSSMB > 0 {
			peakSum += float64(a.PeakRSSMB)
			peakN++
			p.PeakRSSMaxMB = max(p.PeakRSSMaxMB, a.PeakRSSMB)
		}
		if a.CPUSeconds > 0 {
			cpuSum += a.CPUSeconds
			cpuN++
		}
	}
	if p.Runs > 0 {
		p.SuccessRate = float64(p.Successes) / float64(p.Runs)
	}
	if len(durs) > 0 {
		sort.Float64s(durs)
		var sum float64
		for _, d := range durs {
			sum += d
		}
		p.DurationAvgS = sum / float64(len(durs))
		p.DurationP50S = percentile(durs, 0.5)
		p.DurationP90S = percentile(durs, 0.9)
	}
	if peakN > 0 {
		p.PeakRSSAvgMB = peakSum / float64(peakN)
	}
	if cpuN > 0 {
		p.CPUSecondsAvg = cpuSum / float64(cpuN)
	}
	for cat, n := range fails {
		p.CommonFailures = append(p.CommonFailures, FailureCount{Category: cat, N: n})
	}
	sort.Slice(p.CommonFailures, func(i, j int) bool {
		if p.CommonFailures[i].N != p.CommonFailures[j].N {
			return p.CommonFailures[i].N > p.CommonFailures[j].N
		}
		return p.CommonFailures[i].Category < p.CommonFailures[j].Category
	})
	if len(p.CommonFailures) > 3 {
		p.CommonFailures = p.CommonFailures[:3]
	}
	return p
}

// attemptDuration is the recorded duration, else ended − started, in seconds.
func attemptDuration(a Attempt) float64 {
	if a.DurationS != nil && *a.DurationS > 0 {
		return float64(*a.DurationS)
	}
	if a.StartedAt != nil && a.EndedAt != nil {
		return a.EndedAt.Sub(*a.StartedAt).Seconds()
	}
	return 0
}

// percentile of sorted values (nearest rank).
func percentile(sorted []float64, q float64) float64 {
	i := int(q*float64(len(sorted))+0.5) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}
