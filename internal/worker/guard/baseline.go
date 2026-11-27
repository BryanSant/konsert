// Package guard keeps agent work from locking up the machine it runs on, without
// hoarding resources (docs/design/guardrails.md).
//
// Nothing here sets a hard per-session memory cap. Every managed session draws on one
// shared pool, TotalRAM − SurvivalReserve. The OS gets soft signals (a cgroup v2
// MemoryHigh on a shared slice on Linux, CPU weight, background QoS on macOS), and a
// daemon-side watchdog sheds the most recently started session when available
// physical memory stays below the reserve. A shed session is frozen first, so
// allocation stops at once, then killed, and its owner reports memory_pressure_shed
// so the hub requeues the work instead of failing it.
package guard

import (
	"fmt"
	"strings"
)

// Mode is a machine's guardrail mode.
type Mode string

const (
	// ModeAuto resolves from the machine's environment (DefaultMode).
	ModeAuto Mode = "auto"
	// ModePassthrough tracks sessions (process group / Job Object) and cleans up
	// orphans only: no watchdog, no nested limits. Opt-in (config or directive).
	ModePassthrough Mode = "passthrough"
	// ModeAdaptive adds the pressure watchdog and worker deprioritisation, with no
	// kernel watermarks. The default in VMs, which have an out-of-band reboot, and in
	// containers, where it judges against the container's own memory limit.
	ModeAdaptive Mode = "adaptive"
	// ModeAdaptiveStrict adds soft kernel watermarks (Linux slice MemoryHigh and swap
	// cap). The default on bare metal, where a lockup needs a power cycle.
	ModeAdaptiveStrict Mode = "adaptive_strict"
)

// ParseMode accepts auto, passthrough, adaptive and adaptive_strict (case-insensitive;
// "" means auto).
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "", ModeAuto:
		return ModeAuto, nil
	case ModePassthrough, ModeAdaptive, ModeAdaptiveStrict:
		return m, nil
	default:
		return "", fmt.Errorf("unknown guardrail mode %q (want auto, passthrough, adaptive or adaptive_strict)", s)
	}
}

// Strictness orders concrete modes: passthrough < adaptive < adaptive_strict.
// Unknown and auto modes rank -1.
func Strictness(m Mode) int {
	switch m {
	case ModePassthrough:
		return 0
	case ModeAdaptive:
		return 1
	case ModeAdaptiveStrict:
		return 2
	default:
		return -1
	}
}

// DefaultMode is the mode for a MachineFacts.Virtualization class.
func DefaultMode(virtualization string) Mode {
	switch virtualization {
	case "bare_metal":
		return ModeAdaptiveStrict
	default:
		// vm, wsl (the Windows host owns physical memory), unknown, and containers: the
		// container OOM killer fires at the container's limit and picks the biggest
		// process, which may be the agent, so the guard sheds first (against that limit).
		return ModeAdaptive
	}
}

const (
	mib = uint64(1) << 20

	reserveFloor       = 96 * mib
	reserveCapDesktop  = 1536 * mib
	reserveCapHeadless = 768 * mib
)

// ComputeSurvivalReserve is the memory kept free for the OS, sshd and syred:
// 5% of RAM, at least 96 MiB (so a 1 GiB VM stays ~90% usable) and at most 1.5 GiB
// (768 MiB on a headless host, which has no compositor to keep fed).
func ComputeSurvivalReserve(totalRAM uint64, desktop bool) uint64 {
	limit := uint64(reserveCapDesktop)
	if !desktop {
		limit = reserveCapHeadless
	}
	r := totalRAM / 20
	r = max(r, reserveFloor)
	r = min(r, limit)
	if totalRAM > 0 {
		r = min(r, totalRAM/2) // a tiny box keeps at least half for work
	}
	return r
}

// Baseline is what the watchdog needs to know about the host.
type Baseline struct {
	TotalRAM       uint64
	Reserve        uint64
	Desktop        bool
	Virtualization string
	AutoMode       Mode
}

// NewBaseline computes the reserve and auto mode for a host.
func NewBaseline(totalRAM uint64, desktop bool, virtualization string) Baseline {
	return Baseline{
		TotalRAM:       totalRAM,
		Reserve:        ComputeSurvivalReserve(totalRAM, desktop),
		Desktop:        desktop,
		Virtualization: virtualization,
		AutoMode:       DefaultMode(virtualization),
	}
}
