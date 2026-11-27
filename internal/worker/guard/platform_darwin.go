//go:build darwin

package guard

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// macOS has no cgroups. Sessions are process groups. The kernel's own memory-pressure
// level drives the guard: WARN moves worker trees to background QoS (E-cores,
// throttled I/O), CRITICAL sheds. The level is polled through sysctl because syred
// builds with CGO_ENABLED=0, which rules out DISPATCH_SOURCE_TYPE_MEMORYPRESSURE.

const (
	prioDarwinProcess = 4      // PRIO_DARWIN_PROCESS
	prioDarwinBG      = 0x1000 // PRIO_DARWIN_BG
	workerNice        = 5
	syredNice         = -10
)

type darwinPlat struct {
	pageSize uint64
	memTotal uint64
}

func nativePlatform() platform {
	p := &darwinPlat{pageSize: 16384}
	if ps, err := unix.SysctlUint32("vm.pagesize"); err == nil && ps > 0 {
		p.pageSize = uint64(ps)
	}
	p.memTotal, _ = unix.SysctlUint64("hw.memsize")
	return p
}

func (p *darwinPlat) mechanism(Mode) string                   { return "process-group" }
func (p *darwinPlat) configure(Mode, Baseline, throttleState) {}
func (p *darwinPlat) wrap(*Launch, Mode)                      {}
func (p *darwinPlat) release(s *Session)                      { s.plat = nil }
func (p *darwinPlat) throttle(*Session, throttleState)        {} // background() covers it
func (p *darwinPlat) oomKills() (uint64, bool)                { return 0, false }

// treeOf is the session's process tree (tree_unix.go).
func treeOf(s *Session) *procTree {
	t, _ := s.plat.(*procTree)
	return t
}

// peak keeps the process tree current on each watchdog tick (so descendants are known
// before their parents exit); the peak itself comes from the root's rusage.
func (p *darwinPlat) peak(s *Session) uint64 {
	treeOf(s).members(false)
	return 0
}

func (p *darwinPlat) attach(s *Session, m Mode) {
	s.plat = newProcTree(s.pid)
	if m == ModePassthrough {
		return
	}
	_ = unix.Setpriority(unix.PRIO_PROCESS, s.pid, workerNice)
}

// pressureLevel maps kern.memorystatus_vm_pressure_level (1 normal, 2 warn, 4 critical).
func pressureLevel(v uint32) PressureLevel {
	switch {
	case v >= 4:
		return LevelCritical
	case v >= 2:
		return LevelWarn
	case v >= 1:
		return LevelNormal
	default:
		return LevelUnknown
	}
}

func (p *darwinPlat) sample() Signal {
	sig := Signal{PoolStall: -1}
	if v, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level"); err == nil {
		sig.Level = pressureLevel(v)
	}
	if used, ok := p.memUsed(); ok && p.memTotal > 0 {
		sig.Avail, sig.AvailOK = p.memTotal-min(used, p.memTotal), true
	}
	// struct xsw_usage { u_int64_t xsu_total, xsu_avail, xsu_used; ... }.
	if raw, err := unix.SysctlRaw("vm.swapusage"); err == nil && len(raw) >= 24 {
		sig.SwapUsed, sig.SwapOK = binary.NativeEndian.Uint64(raw[16:24]), true
	}
	return sig
}

// memUsed is Activity Monitor's "Memory Used": app memory + wired + compressed.
func (p *darwinPlat) memUsed() (uint64, bool) {
	internal, ok1 := sysctlCount("vm.page_pageable_internal_count")
	purgeable, ok2 := sysctlCount("vm.page_purgeable_count")
	wired, ok3 := sysctlCount("vm.page_wired_count")
	if !ok1 || !ok2 || !ok3 {
		return 0, false
	}
	compressed, _ := sysctlCount("vm.compressor_bytes_used")
	app := internal
	if purgeable < app {
		app -= purgeable
	}
	return (app+wired)*p.pageSize + compressed, true
}

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

func signalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return errors.New("no process")
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		return syscall.Kill(pid, sig)
	}
	return nil
}

func (p *darwinPlat) freeze(s *Session) error {
	if treeOf(s).signal(syscall.SIGSTOP) {
		return nil
	}
	return signalGroup(s.pid, syscall.SIGSTOP)
}

func (p *darwinPlat) kill(s *Session) error {
	if treeOf(s).signal(syscall.SIGKILL) {
		return nil
	}
	return signalGroup(s.pid, syscall.SIGKILL)
}

// rss sums the tree's resident memory with one ps call (only used at shed time).
func (p *darwinPlat) rss(s *Session) uint64 {
	pids := treeOf(s).members(true)
	if len(pids) == 0 {
		return 0
	}
	list := make([]string, len(pids))
	for i, pid := range pids {
		list[i] = strconv.Itoa(pid)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "rss=", "-p", strings.Join(list, ",")).Output()
	if err != nil && len(out) == 0 {
		return 0
	}
	var kb uint64
	for f := range strings.FieldsSeq(string(out)) {
		if v, err := strconv.ParseUint(f, 10, 64); err == nil {
			kb += v
		}
	}
	return kb << 10
}

// background applies PRIO_DARWIN_BG (what `taskpolicy -b -p` does) to every process in
// the tree; taskpolicy takes one pid, not a group.
func (p *darwinPlat) background(s *Session, on bool) {
	prio := 0
	if on {
		prio = prioDarwinBG
	}
	for _, pid := range treeOf(s).members(true) {
		_ = unix.Setpriority(prioDarwinProcess, pid, prio)
	}
}

func (p *darwinPlat) selfProtect() []string {
	if err := unix.Setpriority(unix.PRIO_PROCESS, 0, syredNice); err != nil {
		return []string{fmt.Sprintf("syred nice %d refused (needs root); workers run at nice +%d instead", syredNice, workerNice)}
	}
	return []string{fmt.Sprintf("syred nice %d; workers nice +%d", syredNice, workerNice)}
}

func hostTotalRAM() uint64 {
	v, _ := unix.SysctlUint64("hw.memsize")
	return v
}
