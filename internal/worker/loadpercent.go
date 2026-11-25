//go:build linux || darwin

package worker

// loadPercent estimates CPU % as the load average over the core count (capped at 100).
func loadPercent(load, cores float64) float64 {
	if cores <= 0 || load <= 0 {
		return 0
	}
	return min(100, load/cores*100)
}
