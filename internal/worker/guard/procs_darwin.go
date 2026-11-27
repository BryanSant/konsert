//go:build darwin

package guard

import "golang.org/x/sys/unix"

// listProcs lists every process (kern.proc.all).
func listProcs() ([]procInfo, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]procInfo, 0, len(kps))
	for _, kp := range kps {
		st := kp.Proc.P_starttime
		out = append(out, procInfo{
			pid: int(kp.Proc.P_pid), ppid: int(kp.Eproc.Ppid), pgid: int(kp.Eproc.Pgid),
			start: uint64(st.Sec)*1_000_000 + uint64(st.Usec),
		})
	}
	return out, nil
}
