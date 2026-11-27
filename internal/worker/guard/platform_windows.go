//go:build windows

package guard

import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/BryanSant/syre/internal/worker/proctree"
	"golang.org/x/sys/windows"
)

// Windows: one Job Object per managed session, in every mode.
//
//   - JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: no orphaned grandchildren, ever.
//   - CPU rate control, weight-based (5 normal, 1 throttled); a hard rate only for an
//     explicit cpu_ceiling_pct.
//   - Basic UI restrictions for headless ticket runs only (no logoff, display or
//     system-parameter changes, no desktop switching). Clipboard and handles stay
//     open so agents that drive a browser keep working.
//
// Never JOB_OBJECT_LIMIT_JOB_MEMORY or _PROCESS_MEMORY: they cap committed virtual
// memory, which crashes JVM, .NET, V8/Node and Playwright while physical RAM is free.
// Memory is guarded by the physical-RAM watchdog (GlobalMemoryStatusEx) instead.

const (
	cpuRateEnable     = 0x1
	cpuRateWeight     = 0x2
	cpuRateHardCap    = 0x4
	cpuWeightNormal   = 5
	cpuWeightThrottle = 1

	// forbiddenMemoryLimits must never be set on a worker job (tested).
	forbiddenMemoryLimits = windows.JOB_OBJECT_LIMIT_JOB_MEMORY | windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY

	headlessUILimits = windows.JOB_OBJECT_UILIMIT_EXITWINDOWS | windows.JOB_OBJECT_UILIMIT_DISPLAYSETTINGS |
		windows.JOB_OBJECT_UILIMIT_SYSTEMPARAMETERS | windows.JOB_OBJECT_UILIMIT_DESKTOP
)

var (
	modKernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx    = modKernel32.NewProc("GlobalMemoryStatusEx")
	procK32GetProcessMemoryInfo = modKernel32.NewProc("K32GetProcessMemoryInfo")
	procIsProcessInJob          = modKernel32.NewProc("IsProcessInJob")
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// jobCPURate is JOBOBJECT_CPU_RATE_CONTROL_INFORMATION (the union read as one DWORD).
type jobCPURate struct {
	ControlFlags uint32
	Value        uint32
}

// processMemoryCounters is PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

type winPlat struct{}

type winSess struct {
	mu     sync.Mutex
	job    windows.Handle
	root   windows.Handle // kept open so the root's counters stay readable after it exits
	flags  uint32         // current basic LimitFlags
	bg     bool
	peak   uint64                    // highest resident memory seen (see peak)
	extras map[uint32]windows.Handle // escaped descendants Windows would not put in the job (adopt_windows.go)

	adoptMu   sync.Mutex        // serializes adopt; guards known
	known     map[uint32]uint64 // every process seen in the session -> creation time
	lastAdopt time.Time
}

func nativePlatform() platform { return winPlat{} }

func (winPlat) mechanism(Mode) string                   { return "job-object" }
func (winPlat) configure(Mode, Baseline, throttleState) {}
func (winPlat) wrap(*Launch, Mode)                      {}

// jobLimitFlags is the basic limit set for a worker job.
func jobLimitFlags(background bool) uint32 {
	f := uint32(windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE)
	if background {
		f |= windows.JOB_OBJECT_LIMIT_PRIORITY_CLASS
	}
	return f &^ forbiddenMemoryLimits
}

func setJobLimits(job windows.Handle, flags uint32, background bool) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = flags &^ forbiddenMemoryLimits
	if background {
		info.BasicLimitInformation.PriorityClass = windows.BELOW_NORMAL_PRIORITY_CLASS
	}
	_, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	return err
}

func setJobCPU(job windows.Handle, t throttleState) error {
	rate := jobCPURate{ControlFlags: cpuRateEnable | cpuRateWeight, Value: cpuWeightNormal}
	if t.on {
		rate.Value = cpuWeightThrottle
	}
	if t.cpuPct > 0 {
		rate = jobCPURate{ControlFlags: cpuRateEnable | cpuRateHardCap, Value: uint32(t.cpuPct * 100)}
	}
	_, err := windows.SetInformationJobObject(job, windows.JobObjectCpuRateControlInformation,
		uintptr(unsafe.Pointer(&rate)), uint32(unsafe.Sizeof(rate)))
	return err
}

func (winPlat) attach(s *Session, m Mode) {
	ws := &winSess{extras: map[uint32]windows.Handle{}, known: map[uint32]uint64{}}
	s.plat = ws
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	ws.flags = jobLimitFlags(false)
	if err := setJobLimits(job, ws.flags, false); err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	if m != ModePassthrough {
		_ = setJobCPU(job, throttleState{})
	}
	if s.spec.Headless {
		ui := windows.JOBOBJECT_BASIC_UI_RESTRICTIONS{UIRestrictionsClass: headlessUILimits}
		_, _ = windows.SetInformationJobObject(job, windows.JobObjectBasicUIRestrictions,
			uintptr(unsafe.Pointer(&ui)), uint32(unsafe.Sizeof(ui)))
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(s.pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	defer windows.CloseHandle(proc)
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	ws.job = job
	if root, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(s.pid)); err == nil {
		ws.root = root
	}
}

func winOf(s *Session) *winSess {
	ws, _ := s.plat.(*winSess)
	return ws
}

func (ws *winSess) handle() windows.Handle {
	if ws == nil {
		return 0
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.job
}

// release closes the job: KILL_ON_JOB_CLOSE ends anything the root left behind.
func (winPlat) release(s *Session) {
	ws := winOf(s)
	if ws == nil {
		return
	}
	// Closing the job ends what the root left behind in it; escaped processes the
	// guard adopted go the same way.
	ws.adopt(true)
	ws.killExtras()
	ws.mu.Lock()
	job, root, extras := ws.job, ws.root, ws.extras
	ws.job, ws.root, ws.extras = 0, 0, map[uint32]windows.Handle{}
	ws.mu.Unlock()
	if job != 0 {
		_ = windows.CloseHandle(job)
	}
	if root != 0 {
		_ = windows.CloseHandle(root)
	}
	for _, h := range extras {
		_ = windows.CloseHandle(h)
	}
}

func (winPlat) sample() Signal {
	sig := Signal{PoolStall: -1}
	ms := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); r != 0 {
		sig.Avail, sig.AvailOK = ms.AvailPhys, true
	}
	return sig
}

func (winPlat) freeze(s *Session) error {
	ws := winOf(s)
	job := ws.handle()
	if job == 0 {
		return proctree.Suspend(s.pid)
	}
	ws.adopt(true)
	if ws.suspendAll(job) == 0 {
		return proctree.ErrNoProcess
	}
	return nil
}

func (winPlat) kill(s *Session) error {
	ws := winOf(s)
	job := ws.handle()
	if job == 0 {
		return errors.New("no job object")
	}
	ws.adopt(true)
	err := windows.TerminateJobObject(job, 1)
	ws.killExtras()
	return err
}

// jobPIDs lists the processes in a job.
func jobPIDs(job windows.Handle) []uint32 {
	const maxIDs = 1024
	buf := make([]byte, 8+maxIDs*unsafe.Sizeof(uintptr(0)))
	if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&buf[0])), uint32(len(buf)), nil); err != nil {
		return nil
	}
	n := *(*uint32)(unsafe.Pointer(&buf[4]))
	ids := unsafe.Slice((*uintptr)(unsafe.Pointer(&buf[8])), min(n, maxIDs))
	out := make([]uint32, len(ids))
	for i, id := range ids {
		out[i] = uint32(id)
	}
	return out
}

// memCounters reads a process's PROCESS_MEMORY_COUNTERS (works on an exited process
// while a handle to it is open).
func memCounters(h windows.Handle) (processMemoryCounters, bool) {
	pmc := processMemoryCounters{CB: uint32(unsafe.Sizeof(processMemoryCounters{}))}
	r, _, _ := procK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.CB))
	return pmc, r != 0
}

// workingSet sums the working sets of the session's live processes (job members and
// adopted ones) now, and returns the largest single-process peak working set among
// them (adopted processes count even after they exit).
func (ws *winSess) workingSet(job windows.Handle) (sum, maxPeak uint64) {
	add := func(h windows.Handle, live bool) {
		if pmc, ok := memCounters(h); ok {
			if live {
				sum += uint64(pmc.WorkingSetSize)
			}
			maxPeak = max(maxPeak, uint64(pmc.PeakWorkingSetSize))
		}
	}
	for _, pid := range jobPIDs(job) {
		if h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid); err == nil {
			add(h, true)
			_ = windows.CloseHandle(h)
		}
	}
	for _, h := range ws.extraHandles() {
		add(h, alive(h))
	}
	return sum, maxPeak
}

// rss sums the working sets of the session's processes.
func (winPlat) rss(s *Session) uint64 {
	ws := winOf(s)
	job := ws.handle()
	if job == 0 {
		return 0
	}
	ws.adopt(true)
	sum, _ := ws.workingSet(job)
	return sum
}

// peak is the tree's peak resident memory. Windows keeps no peak working set for a
// job (PeakJobMemoryUsed is committed memory, which overstates a JVM or Node that
// reserves far more than it touches), so it is the highest of: the job's summed
// working sets at each watchdog sample, any one process's own peak working set, and
// the root's peak working set (readable after exit through the handle kept open).
func (winPlat) peak(s *Session) uint64 {
	ws := winOf(s)
	if ws == nil {
		return 0
	}
	ws.mu.Lock()
	job, root := ws.job, ws.root
	ws.mu.Unlock()
	var v uint64
	if job != 0 {
		ws.adopt(false)
		sum, maxPeak := ws.workingSet(job)
		v = max(sum, maxPeak)
	}
	if root != 0 {
		if pmc, ok := memCounters(root); ok {
			v = max(v, uint64(pmc.PeakWorkingSetSize))
		}
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.peak = max(ws.peak, v)
	return ws.peak
}

// background sets the job's priority class to below normal (and back).
func (winPlat) background(s *Session, on bool) {
	ws := winOf(s)
	if ws == nil {
		return
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.job == 0 || ws.bg == on {
		return
	}
	flags := jobLimitFlags(on)
	if setJobLimits(ws.job, flags, on) == nil {
		ws.flags, ws.bg = flags, on
	}
}

func (winPlat) throttle(s *Session, t throttleState) {
	if job := winOf(s).handle(); job != 0 {
		_ = setJobCPU(job, t)
	}
}

func (winPlat) selfProtect() []string {
	if err := windows.SetPriorityClass(windows.CurrentProcess(), windows.ABOVE_NORMAL_PRIORITY_CLASS); err != nil {
		return []string{fmt.Sprintf("syred above-normal priority refused: %v", err)}
	}
	return []string{"syred above-normal priority; worker jobs CPU weight 5 (1 throttled), below normal while throttled"}
}

// Job exposes a session's Job Object to callers that need the handle (ConPTY
// teardown). 0 when none.
func (s *Session) Job() windows.Handle {
	if s == nil {
		return 0
	}
	return winOf(s).handle()
}

func hostTotalRAM() uint64 {
	ms := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); r != 0 {
		return ms.TotalPhys
	}
	return 0
}
