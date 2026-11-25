//go:build windows

package worker

import (
	"unsafe"

	"github.com/BryanSant/syre/internal/protocol"
	"golang.org/x/sys/windows"
)

// vitalsSampler reads Windows vitals with two kernel32 calls: GetSystemTimes (CPU % as
// the busy share since the previous sample) and GlobalMemoryStatusEx (memory, and the
// page file as swap). Windows has no load average.
type vitalsSampler struct {
	prevIdle, prevTotal uint64
}

var (
	modKernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes       = modKernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = modKernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx is MEMORYSTATUSEX.
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

func newVitalsSampler() *vitalsSampler { return &vitalsSampler{} }

func filetime64(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

func (s *vitalsSampler) sample(m *protocol.WorkerMetrics) {
	var idle, kernel, user windows.Filetime
	if r, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)),
	); r != 0 {
		// Kernel time includes idle time.
		i, total := filetime64(idle), filetime64(kernel)+filetime64(user)
		if s.prevTotal > 0 && total > s.prevTotal && i >= s.prevIdle {
			dt, di := total-s.prevTotal, i-s.prevIdle
			if di <= dt {
				m.CPUPercent = float64(dt-di) / float64(dt) * 100
			}
		}
		s.prevIdle, s.prevTotal = i, total
	}
	ms := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); r != 0 {
		m.MemTotalMB = ms.TotalPhys / (1 << 20)
		usedPhys := ms.TotalPhys - min(ms.AvailPhys, ms.TotalPhys)
		m.MemUsedMB = usedPhys / (1 << 20)
		// The commit limit is RAM plus the page files; what is committed beyond RAM is
		// (roughly) what lives in the page file.
		if ms.TotalPageFile > ms.TotalPhys {
			m.SwapTotalMB = (ms.TotalPageFile - ms.TotalPhys) / (1 << 20)
			committed := ms.TotalPageFile - min(ms.AvailPageFile, ms.TotalPageFile)
			if committed > usedPhys {
				m.SwapUsedMB = min((committed-usedPhys)/(1<<20), m.SwapTotalMB)
			}
		}
	}
}
