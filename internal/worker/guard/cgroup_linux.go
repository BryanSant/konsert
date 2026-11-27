//go:build linux

package guard

import "os"

// selfCgroup is this process's cgroup v2 path ("" unknown).
func selfCgroup() string {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	return cgroupV2Path(raw)
}

// CgroupMemoryLimit is the tightest memory.max above syred's own cgroup (a container's
// limit), or 0 when none is set. Machine facts report it as the RAM total when it is
// below the host's.
func CgroupMemoryLimit() uint64 {
	if l, ok := tightestLimit(cgroupRoot, selfCgroup()); ok {
		return l.limit
	}
	return 0
}

// oomKills is the kernel's count of OOM kills since boot (/proc/vmstat oom_kill),
// global and memcg alike, so a container's OOM killer counts too.
func (p *linuxPlat) oomKills() (uint64, bool) {
	raw, err := os.ReadFile("/proc/vmstat")
	if err != nil {
		return 0, false
	}
	return statField(raw, "oom_kill")
}
