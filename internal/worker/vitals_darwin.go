//go:build darwin

package worker

import (
	"encoding/binary"
	"runtime"

	"github.com/BryanSant/konsert/internal/protocol"
	"golang.org/x/sys/unix"
)

// vitalsSampler reads macOS vitals with sysctl(3) only: no subprocess, a dozen system
// calls (~tens of µs). CPU % is the 1-minute load average over the core count (a real
// per-tick CPU % needs host_statistics, i.e. cgo).
type vitalsSampler struct {
	cores    float64
	pageSize uint64
	memTotal uint64 // bytes
}

func newVitalsSampler() *vitalsSampler {
	s := &vitalsSampler{cores: float64(runtime.NumCPU()), pageSize: 16384}
	if ps, err := unix.SysctlUint32("vm.pagesize"); err == nil && ps > 0 {
		s.pageSize = uint64(ps)
	}
	s.memTotal, _ = unix.SysctlUint64("hw.memsize")
	return s
}

func (s *vitalsSampler) sample(m *protocol.WorkerMetrics) {
	// struct loadavg { fixpt_t ldavg[3]; long fscale; }: three uint32, padding, int64.
	if raw, err := unix.SysctlRaw("vm.loadavg"); err == nil && len(raw) >= 24 {
		ld := binary.NativeEndian.Uint32(raw[0:4])
		if fscale := binary.NativeEndian.Uint64(raw[16:24]); fscale > 0 {
			m.LoadAvg = float64(ld) / float64(fscale)
			m.CPUPercent = loadPercent(m.LoadAvg, s.cores)
		}
	}
	m.MemTotalMB = s.memTotal / (1 << 20)
	m.MemUsedMB = s.memUsed() / (1 << 20)
	// struct xsw_usage { u_int64_t xsu_total, xsu_avail, xsu_used; ... }.
	if raw, err := unix.SysctlRaw("vm.swapusage"); err == nil && len(raw) >= 24 {
		m.SwapTotalMB = binary.NativeEndian.Uint64(raw[0:8]) / (1 << 20)
		m.SwapUsedMB = binary.NativeEndian.Uint64(raw[16:24]) / (1 << 20)
	}
}

// memUsed is Activity Monitor's "Memory Used": app memory (anonymous pages that are not
// purgeable) + wired + compressed. It tracks vm_stat's active + wired + compressor pages,
// which konsertd used before, closely (TestVitalsDarwinMatchesVMStat).
func (s *vitalsSampler) memUsed() uint64 {
	internal, ok1 := sysctlCount("vm.page_pageable_internal_count")
	purgeable, ok2 := sysctlCount("vm.page_purgeable_count")
	wired, ok3 := sysctlCount("vm.page_wired_count")
	if !ok1 || !ok2 || !ok3 {
		return 0
	}
	compressed, _ := sysctlCount("vm.compressor_bytes_used")
	app := internal
	if purgeable < app {
		app -= purgeable
	}
	used := (app+wired)*s.pageSize + compressed
	if s.memTotal > 0 && used > s.memTotal {
		used = s.memTotal
	}
	return used
}

// sysctlCount reads an integer sysctl of either width (the VM counters mix int and
// uint64 across macOS versions).
func sysctlCount(name string) (uint64, bool) {
	raw, err := unix.SysctlRaw(name)
	if err != nil {
		return 0, false
	}
	switch len(raw) {
	case 4:
		return uint64(binary.NativeEndian.Uint32(raw)), true
	case 8:
		return binary.NativeEndian.Uint64(raw), true
	}
	return 0, false
}
