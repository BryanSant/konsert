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
	"unsafe"

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
func (p *darwinPlat) commit(*Session) uint64                  { return 0 }

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

// rss is the tree's memory footprint (only used at shed time and for victim choice).
// Resident memory misleads here: macOS compresses and swaps a runaway, so a hog holding
// 12 GiB showed 921 MiB of RSS live and was called a bystander. phys_footprint counts
// compressed and swapped pages, as Activity Monitor's "Memory" column does. ps RSS
// covers processes whose footprint can't be read.
func (p *darwinPlat) rss(s *Session) uint64 {
	pids := treeOf(s).members(true)
	var sum uint64
	var rest []string
	for _, pid := range pids {
		if v, ok := physFootprint(pid); ok {
			sum += v
		} else {
			rest = append(rest, strconv.Itoa(pid))
		}
	}
	if len(rest) == 0 {
		return sum
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "rss=", "-p", strings.Join(rest, ",")).Output()
	if err != nil && len(out) == 0 {
		return sum
	}
	for f := range strings.FieldsSeq(string(out)) {
		if v, err := strconv.ParseUint(f, 10, 64); err == nil {
			sum += v << 10
		}
	}
	return sum
}

const (
	procInfoCallPIDRusage = 9 // PROC_INFO_CALL_PIDRUSAGE
	rusageInfoV4          = 4 // RUSAGE_INFO_V4
	// Offset of ri_phys_footprint in struct rusage_info_v4: a 16-byte uuid, then
	// user_time, system_time, pkg_idle_wkups, interrupt_wkups, pageins, wired_size and
	// resident_size (uint64 each).
	footprintOffset = 16 + 7*8
)

// physFootprint is proc_pid_rusage(pid, RUSAGE_INFO_V4).ri_phys_footprint, called
// through the proc_info syscall because syred builds without cgo.
func physFootprint(pid int) (uint64, bool) {
	var buf [512]byte // rusage_info_v4 is 296 bytes
	//lint:ignore SA1019 x/sys has no proc_pid_rusage wrapper, and syred builds without cgo.
	_, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDRusage, uintptr(pid), rusageInfoV4, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return 0, false
	}
	return binary.NativeEndian.Uint64(buf[footprintOffset:]), true
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
