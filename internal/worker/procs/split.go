package procs

import "sort"

// Split is a sample's load divided between agent work (the attributed sessions, plus
// syred itself and its helpers) and everything else on the machine.
type Split struct {
	AgentCPUPercent, OtherCPUPercent float64
	AgentRSS, OtherRSS               uint64
	// TopOther are the busiest processes outside agent work, by CPU then memory.
	TopOther []Proc
	// Unreported counts processes the OS reported no CPU or memory for (macOS: other
	// users' processes without root); their load is in neither side.
	Unreported int
}

// SplitLoad divides smp: usage is the attribution of the same sample and self the
// daemon's PID (its descendants that belong to no session are agent work too).
func SplitLoad(smp Sample, usage []SessionUsage, self, topN int) Split {
	agent := map[int]bool{}
	for _, u := range usage {
		for _, pid := range u.PIDs {
			agent[pid] = true
		}
	}
	kids := map[int][]int{}
	for _, p := range smp.Procs {
		if p.PPID != p.PID {
			kids[p.PPID] = append(kids[p.PPID], p.PID)
		}
	}
	var mark func(int)
	mark = func(pid int) {
		if agent[pid] && pid != self {
			return
		}
		agent[pid] = true
		for _, c := range kids[pid] {
			mark(c)
		}
	}
	if self > 0 {
		mark(self)
	}
	var sp Split
	var other []Proc
	for _, p := range smp.Procs {
		switch {
		case p.Partial:
			sp.Unreported++
		case agent[p.PID]:
			sp.AgentCPUPercent += p.CPUPercent
			sp.AgentRSS += p.RSS
		default:
			sp.OtherCPUPercent += p.CPUPercent
			sp.OtherRSS += p.RSS
			other = append(other, p)
		}
	}
	sort.Slice(other, func(i, j int) bool {
		if other[i].CPUPercent != other[j].CPUPercent {
			return other[i].CPUPercent > other[j].CPUPercent
		}
		return other[i].RSS > other[j].RSS
	})
	sp.TopOther = other[:min(topN, len(other))]
	return sp
}
