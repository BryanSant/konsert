//go:build linux

package guard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Linux: every managed session runs in its own transient systemd user scope inside one
// shared syre-workers.slice. Limits live on the slice, never on a scope, so the
// sessions share the headroom (docs/design/guardrails.md):
//
//	adaptive_strict  MemoryHigh = Total − Reserve (soft: reclaim and throttle, never
//	                 kill), MemorySwapMax = min(25% RAM, 2 GiB), no MemoryMax,
//	                 CPUWeight 100 (20 throttled), TasksMax 16384, CPUQuota only for
//	                 an explicit cpu_ceiling_pct.
//	adaptive         the same scopes for tracking, peak and tree kill; no memory
//	                 watermarks.
//
// Without a usable user manager (minimal WSL2, containers, no session bus) sessions
// stay plain process groups and the /proc watchdog is the only guard. Inside a
// container the watchdog judges against the container's memory.max (cgroup_linux.go). No rlimits:
// RLIMIT_AS caps address space, which kills healthy JVM/V8 processes.

const (
	workerSlice     = "syre-workers.slice"
	workerTasksMax  = 16384
	workerNice      = 5
	workerNiceSlow  = 10
	workerOOMAdj    = 300
	syredOOMAdj     = -500
	swapCapBytes    = 2 << 30
	cgroupRoot      = "/sys/fs/cgroup"
	cgroupDiscovery = 3 * time.Second
)

type linuxPlat struct {
	probeOnce   sync.Once
	cleanupOnce sync.Once
	scopeOK     bool
	runPath     string
	ctlPath     string
	seq         atomic.Int64

	mu        sync.Mutex
	slicePath string // absolute cgroupfs path of the worker slice, once discovered
	limitFrom string // the cgroup path limit was looked up from
	limit     cgLimit
	limitOK   bool
}

// workerLimit is the tightest memory.max above the workers: from the worker slice once
// known, else from syred's own cgroup (workers without scopes stay in it). The walk is
// redone only when its starting point changes.
func (p *linuxPlat) workerLimit() (cgLimit, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	from := ""
	if p.slicePath != "" {
		from, _ = filepath.Rel(cgroupRoot, p.slicePath)
		from = "/" + from
	} else {
		from = selfCgroup()
	}
	if from != p.limitFrom || from == "" {
		p.limitFrom = from
		p.limit, p.limitOK = tightestLimit(cgroupRoot, from)
	}
	return p.limit, p.limitOK
}

type linuxSess struct {
	mu     sync.Mutex
	cgroup string // absolute cgroupfs path of the session's scope ("" unknown)
	peak   uint64 // highest memory.peak / memory.current seen
	nice   int
	tree   *procTree // descendants, for when there is no scope (tree_unix.go)
}

func (ls *linuxSess) procTree() *procTree {
	if ls == nil {
		return nil
	}
	return ls.tree
}

func nativePlatform() platform { return &linuxPlat{} }

// probe checks once whether transient user scopes work here.
func (p *linuxPlat) probe() bool {
	p.probeOnce.Do(func() {
		run, err1 := exec.LookPath("systemd-run")
		ctl, err2 := exec.LookPath("systemctl")
		if err1 != nil || err2 != nil {
			return
		}
		if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
			return // not cgroup v2
		}
		if os.Getenv("XDG_RUNTIME_DIR") == "" && os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, run, "--user", "--scope", "--quiet", "--collect",
			"--slice="+workerSlice, "--unit="+p.unitName("probe"), "--", "true")
		if err := cmd.Run(); err != nil {
			return
		}
		p.runPath, p.ctlPath, p.scopeOK = run, ctl, true
	})
	return p.scopeOK
}

func (p *linuxPlat) unitName(kind string) string {
	k := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, strings.ToLower(kind))
	if k == "" {
		k = "session"
	}
	return fmt.Sprintf("syre-%s-%d-%d.scope", k, os.Getpid(), p.seq.Add(1))
}

func (p *linuxPlat) mechanism(m Mode) string {
	if m == ModePassthrough {
		return "process-group"
	}
	if p.probe() {
		return "systemd-scope"
	}
	return "process-group"
}

// sliceProps is the slice configuration for a mode; exported shape for tests.
func sliceProps(m Mode, b Baseline, t throttleState, cpus int) []string {
	weight := 100
	if t.on {
		weight = 20
	}
	quota := "CPUQuota="
	if t.cpuPct > 0 {
		quota = fmt.Sprintf("CPUQuota=%d%%", t.cpuPct*max(cpus, 1))
	}
	props := []string{
		fmt.Sprintf("CPUWeight=%d", weight),
		quota,
		fmt.Sprintf("TasksMax=%d", workerTasksMax),
		"MemoryMax=infinity",
	}
	if m == ModeAdaptiveStrict && b.TotalRAM > b.Reserve && b.TotalRAM > 0 {
		swap := min(b.TotalRAM/4, uint64(swapCapBytes))
		props = append(props,
			fmt.Sprintf("MemoryHigh=%d", b.TotalRAM-b.Reserve),
			fmt.Sprintf("MemorySwapMax=%d", swap))
	} else {
		props = append(props, "MemoryHigh=infinity", "MemorySwapMax=infinity")
	}
	return props
}

func (p *linuxPlat) configure(m Mode, b Baseline, t throttleState) {
	if m == ModePassthrough || !p.probe() {
		return
	}
	p.cleanupOnce.Do(p.stopStaleScopes)
	args := append([]string{"--user", "set-property", "--runtime", workerSlice}, sliceProps(m, b, t, runtime.NumCPU())...)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, p.ctlPath, args...).Run()
}

// stopStaleScopes stops worker scopes left by a syred that is gone (it crashed or was
// killed): outside syred's own service cgroup they would otherwise run unsupervised
// while the hub requeues their work. Scopes of a live syred (another instance) stay.
func (p *linuxPlat) stopStaleScopes() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, p.ctlPath, "--user", "list-units", "--plain", "--no-legend", "--all", "syre-*.scope").Output()
	if err != nil {
		return
	}
	self, _ := os.Executable()
	var stale []string
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if unit := f[0]; staleScope(unit, self) {
			stale = append(stale, unit)
		}
	}
	if len(stale) > 0 {
		_ = exec.CommandContext(ctx, p.ctlPath, append([]string{"--user", "stop"}, stale...)...).Run()
	}
}

// staleScope reports whether unit (syre-<kind>-<pid>-<seq>.scope) belongs to a syred
// that no longer runs.
func staleScope(unit, selfExe string) bool {
	name := strings.TrimSuffix(unit, ".scope")
	parts := strings.Split(name, "-")
	if len(parts) < 4 || parts[0] != "syre" {
		return false
	}
	pid, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil || pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return false
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return true // no such process
	}
	return filepath.Base(exe) != filepath.Base(selfExe)
}

func (p *linuxPlat) wrap(l *Launch, m Mode) {
	if m == ModePassthrough || !p.probe() {
		return
	}
	// Resolve first: a missing binary must still fail at exec (callers report "not
	// installed"), not as a systemd-run exit status.
	bin, err := exec.LookPath(l.Binary)
	if err != nil {
		return
	}
	unit := p.unitName(l.spec.Kind)
	args := []string{"--user", "--scope", "--quiet", "--collect", "--slice=" + workerSlice, "--unit=" + unit, "--", bin}
	l.Binary = p.runPath
	l.Args = append(args, l.Args...)
	l.unit = unit
}

func (p *linuxPlat) attach(s *Session, m Mode) {
	ls := &linuxSess{nice: workerNice, tree: newProcTree(s.pid)}
	s.plat = ls
	if m == ModePassthrough {
		return
	}
	// Lower the worker rather than raise syred: allowed without privileges, and
	// children forked after this inherit both.
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, s.pid, workerNice)
	_ = os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", s.pid), []byte(strconv.Itoa(workerOOMAdj)), 0)
	if s.unit != "" {
		go p.discoverCgroup(s, ls)
	}
}

// discoverCgroup waits for systemd-run to move the process into its scope.
func (p *linuxPlat) discoverCgroup(s *Session, ls *linuxSess) {
	deadline := time.Now().Add(cgroupDiscovery)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", s.pid))
		if err != nil {
			return // already gone
		}
		if path := cgroupV2Path(raw); strings.HasSuffix(path, "/"+s.unit) {
			abs := filepath.Join(cgroupRoot, path)
			ls.mu.Lock()
			ls.cgroup = abs
			ls.mu.Unlock()
			p.mu.Lock()
			p.slicePath = filepath.Dir(abs)
			p.mu.Unlock()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// cgroupV2Path returns the unified-hierarchy path from /proc/<pid>/cgroup ("0::/x").
func cgroupV2Path(raw []byte) string {
	for line := range bytes.SplitSeq(raw, []byte{'\n'}) {
		if rest, ok := bytes.CutPrefix(line, []byte("0::")); ok {
			return string(bytes.TrimSpace(rest))
		}
	}
	return ""
}

func (p *linuxPlat) release(*Session) {}

func (p *linuxPlat) sample() Signal {
	sig := Signal{PoolStall: -1}
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		if v, ok := meminfoField(raw, "MemAvailable:"); ok {
			sig.Avail, sig.AvailOK = v, true
		}
	}
	if l, ok := p.workerLimit(); ok {
		// A container's limit: its OOM killer fires there, whatever the host has free.
		if v, ok := l.avail(); ok && (!sig.AvailOK || v < sig.Avail) {
			sig.Avail, sig.AvailOK, sig.AvailHard = v, true, true
		}
	}
	p.mu.Lock()
	slice := p.slicePath
	p.mu.Unlock()
	if slice == "" {
		// No scope, so no MemorySwapMax: watch swap growth instead.
		if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
			total, ok1 := meminfoField(raw, "SwapTotal:")
			free, ok2 := meminfoField(raw, "SwapFree:")
			if ok1 && ok2 && total > 0 {
				sig.SwapUsed, sig.SwapOK = total-min(free, total), true
			}
		}
	}
	if slice != "" {
		if raw, err := os.ReadFile(filepath.Join(slice, "memory.pressure")); err == nil {
			if v, ok := psiFullAvg10(raw); ok {
				sig.PoolStall = v
			}
		}
	}
	return sig
}

func meminfoField(raw []byte, key string) (uint64, bool) {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, key) {
			continue
		}
		f := strings.Fields(line[len(key):])
		if len(f) == 0 {
			return 0, false
		}
		kb, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb << 10, true
	}
	return 0, false
}

// psiFullAvg10 parses "full avg10=12.34 ..." from a PSI file.
func psiFullAvg10(raw []byte) (float64, bool) {
	for line := range strings.SplitSeq(string(raw), "\n") {
		if !strings.HasPrefix(line, "full ") {
			continue
		}
		for f := range strings.FieldsSeq(line) {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				x, err := strconv.ParseFloat(v, 64)
				return x, err == nil
			}
		}
	}
	return 0, false
}

func (ls *linuxSess) path() string {
	if ls == nil {
		return ""
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.cgroup
}

func sessOf(s *Session) *linuxSess {
	ls, _ := s.plat.(*linuxSess)
	return ls
}

func (p *linuxPlat) freeze(s *Session) error {
	if cg := sessOf(s).path(); cg != "" {
		if err := os.WriteFile(filepath.Join(cg, "cgroup.freeze"), []byte("1"), 0); err == nil {
			return nil
		}
	}
	if sessOf(s).procTree().signal(syscall.SIGSTOP) {
		return nil
	}
	return signalGroup(s.pid, syscall.SIGSTOP)
}

func (p *linuxPlat) kill(s *Session) error {
	var cgErr error = errors.New("no cgroup")
	if cg := sessOf(s).path(); cg != "" {
		cgErr = os.WriteFile(filepath.Join(cg, "cgroup.kill"), []byte("1"), 0)
	}
	if cgErr == nil {
		_ = signalGroup(s.pid, syscall.SIGKILL)
		return nil
	}
	// No cgroup: the tree covers shells an agent starts in their own process group.
	treeOK := sessOf(s).procTree().signal(syscall.SIGKILL)
	if pgErr := signalGroup(s.pid, syscall.SIGKILL); pgErr != nil && !treeOK {
		return pgErr
	}
	return nil
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

func (p *linuxPlat) rss(s *Session) uint64 {
	ls := sessOf(s)
	if cg := ls.path(); cg != "" {
		if v, ok := readUintFile(filepath.Join(cg, "memory.current")); ok {
			ls.notePeak(v)
			return v
		}
	}
	if pids := ls.procTree().members(true); len(pids) > 0 {
		return pidsRSS(pids)
	}
	return 0
}

func (p *linuxPlat) peak(s *Session) uint64 {
	ls := sessOf(s)
	if ls == nil {
		return 0
	}
	if cg := ls.path(); cg != "" {
		if v, ok := readUintFile(filepath.Join(cg, "memory.peak")); ok {
			ls.notePeak(v)
		} else if v, ok := readUintFile(filepath.Join(cg, "memory.current")); ok {
			ls.notePeak(v)
		}
	} else {
		ls.tree.members(false) // keep descendants known before their parents exit
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.peak
}

func (ls *linuxSess) notePeak(v uint64) {
	if ls == nil {
		return
	}
	ls.mu.Lock()
	ls.peak = max(ls.peak, v)
	ls.mu.Unlock()
}

func (p *linuxPlat) background(*Session, bool) {}

// throttle renices a session that has no scope (the slice's CPUWeight covers the rest).
func (p *linuxPlat) throttle(s *Session, t throttleState) {
	ls := sessOf(s)
	if ls == nil || ls.path() != "" {
		return
	}
	n := workerNice
	if t.on {
		n = workerNiceSlow
	}
	_ = syscall.Setpriority(syscall.PRIO_PGRP, s.pid, n)
}

func (p *linuxPlat) selfProtect() []string {
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte(strconv.Itoa(syredOOMAdj)), 0); err != nil {
		return []string{fmt.Sprintf("syred oom_score_adj %d refused (needs CAP_SYS_RESOURCE); workers run at +%d and nice +%d instead", syredOOMAdj, workerOOMAdj, workerNice)}
	}
	return []string{fmt.Sprintf("syred oom_score_adj %d; workers +%d, nice +%d", syredOOMAdj, workerOOMAdj, workerNice)}
}

func hostTotalRAM() uint64 {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	v, _ := meminfoField(raw, "MemTotal:")
	if l := CgroupMemoryLimit(); l > 0 && l < v {
		return l
	}
	return v
}
