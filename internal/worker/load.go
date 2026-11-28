package worker

import (
	"context"
	"strings"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/procs"
)

// Process attribution (docs/plan.md, Phase 6). The sampler reads the process table
// every procs.BusyInterval while sessions run (procs.IdleInterval otherwise); each
// sample is split among the hosted and observed sessions, and the latest split rides
// on the heartbeat (WorkerMetrics.Load).

// loadLoop samples until ctx ends (one per hub connection, like the shell poller; the
// sampler and attributor outlive connections, so lineage is kept across them).
func (d *Daemon) loadLoop(ctx context.Context) {
	d.loadOnce.Do(func() {
		d.sampler = procs.NewSampler()
		d.attrib = procs.NewAttributor()
	})
	d.sampler.Run(ctx, func() bool { return len(d.shellRoots()) > 0 }, d.attribute)
}

// attribute splits a sample among the sessions and keeps the result for heartbeats.
func (d *Daemon) attribute(smp procs.Sample) {
	usage := d.attrib.Attribute(smp, d.loadRoots())
	load := &protocol.MachineLoad{At: smp.At.UTC()}
	for _, u := range usage {
		sl := protocol.SessionLoad{CPUPercent: u.CPUPercent, RSSMB: u.RSS >> 20, PeakRSSMB: u.PeakRSS >> 20, Procs: u.Procs, Orphans: u.Orphans}
		kind, id, _ := strings.Cut(u.Key, ":")
		switch kind {
		case "task":
			sl.TaskID = id
		case "chat":
			sl.ChatID = id
		case "observed":
			sl.ExternalID = id
		default:
			continue
		}
		load.Sessions = append(load.Sessions, sl)
	}
	d.load.Store(load)
}

// loadRoots lists the sessions to attribute: managed ones first (with what their
// guardrail container holds), then observed ones.
func (d *Daemon) loadRoots() []procs.Root {
	members := d.guard.Members()
	var roots []procs.Root
	for key, pid := range d.shellRoots() {
		roots = append(roots, procs.Root{Key: key, PID: pid, Members: members[pid]})
	}
	if d.discovery != nil {
		for id, pid := range d.discovery.observedPIDs() {
			roots = append(roots, procs.Root{Key: "observed:" + id, PID: pid})
		}
	}
	return roots
}
