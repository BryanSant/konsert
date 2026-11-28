//go:build darwin

package procs

import (
	"bytes"
	"encoding/binary"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// proc_pid_rusage(pid, RUSAGE_INFO_V4) through the proc_info syscall (syred builds
// without cgo). rusage_info_v4 starts with ri_uuid[16], then uint64s: ri_user_time,
// ri_system_time, ri_pkg_idle_wkups, ri_interrupt_wkups, ri_pageins, ri_wired_size,
// ri_resident_size, ri_phys_footprint, …
const (
	procInfoCallPIDRusage = 9
	rusageInfoV4          = 4
	userTimeOffset        = 16
	systemTimeOffset      = 16 + 8
	footprintOffset       = 16 + 7*8
)

// machTick converts rusage times to nanoseconds: they are mach absolute time units,
// which are nanoseconds on Intel and 125/3 ns (a 24 MHz timebase) on Apple silicon.
var machTick = func() float64 {
	if runtime.GOARCH == "arm64" {
		return 125.0 / 3.0
	}
	return 1
}()

func osTable() ([]Proc, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]Proc, 0, len(kps))
	var buf [512]byte // rusage_info_v4 is 296 bytes
	for i := range kps {
		kp := &kps[i]
		pid := int(kp.Proc.P_pid)
		if pid <= 0 {
			continue
		}
		name := kp.Proc.P_comm[:]
		if n := bytes.IndexByte(name, 0); n >= 0 {
			name = name[:n]
		}
		p := Proc{
			PID:   pid,
			PPID:  int(kp.Eproc.Ppid),
			Name:  string(name),
			Start: time.Unix(kp.Proc.P_starttime.Sec, int64(kp.Proc.P_starttime.Usec)*1000),
		}
		if dev := kp.Eproc.Tdev; dev != -1 && dev != 0 {
			p.TTY = uint64(uint32(dev))
		}
		//lint:ignore SA1019 x/sys has no proc_pid_rusage wrapper, and syred builds without cgo.
		_, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDRusage, uintptr(pid), rusageInfoV4, 0,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if errno != 0 {
			p.Partial = true // another user's process (or it just exited)
		} else {
			ticks := binary.NativeEndian.Uint64(buf[userTimeOffset:]) + binary.NativeEndian.Uint64(buf[systemTimeOffset:])
			p.CPU = time.Duration(float64(ticks) * machTick)
			p.RSS = binary.NativeEndian.Uint64(buf[footprintOffset:])
		}
		out = append(out, p)
	}
	return out, nil
}
