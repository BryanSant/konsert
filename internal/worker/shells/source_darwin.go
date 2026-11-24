//go:build darwin

package shells

import (
	"bytes"
	"encoding/binary"
	"time"

	"golang.org/x/sys/unix"
)

// sysctlSource reads the process table with sysctl kern.proc.all (no process spawned
// per tick) and command lines with kern.procargs2. macOS does not expose per-process
// CPU time without cgo (proc_pid_rusage), so busy comes from child processes only.
type sysctlSource struct{}

// OSSource returns the process table of this machine.
func OSSource() Source { return sysctlSource{} }

// szomb is the zombie p_stat.
const szomb = 5

func (sysctlSource) Snapshot() ([]Proc, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]Proc, 0, len(kps))
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
		out = append(out, Proc{
			PID:    pid,
			PPID:   int(kp.Eproc.Ppid),
			PGID:   int(kp.Eproc.Pgid),
			Name:   string(name),
			Start:  time.Unix(kp.Proc.P_starttime.Sec, int64(kp.Proc.P_starttime.Usec)*1000),
			Zombie: kp.Proc.P_stat == szomb,
		})
	}
	return out, nil
}

func (sysctlSource) Details(*Proc) {}

// Argv reads kern.procargs2: argc (int32), the executable path, NUL padding, then argc
// NUL-terminated arguments (then the environment, ignored).
func (sysctlSource) Argv(pid int) []string {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) < 4 {
		return nil
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	n := bytes.IndexByte(rest, 0)
	if n < 0 {
		return nil
	}
	rest = rest[n:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	var argv []string
	for len(argv) < argc && len(rest) > 0 {
		n := bytes.IndexByte(rest, 0)
		if n < 0 {
			argv = append(argv, string(rest))
			break
		}
		argv = append(argv, string(rest[:n]))
		rest = rest[n+1:]
	}
	return argv
}
