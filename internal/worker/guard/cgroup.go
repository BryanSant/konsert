package guard

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A container (or a cgroup with MemoryMax above syred) caps the memory workers can use
// below the host's RAM, and its OOM killer fires at that cap, not when the host runs
// short. The guard judges against the tightest memory.max on the path from the workers'
// cgroup up to the root: TotalRAM is that limit, available memory is the limit minus
// the cgroup's working set (memory.current less inactive file cache, which the kernel
// reclaims before it OOM-kills).

// cgLimit is the tightest memory limit above a cgroup.
type cgLimit struct {
	dir   string // absolute cgroupfs dir holding the limit
	limit uint64
}

// tightestLimit walks from cgroup path rel (as in /proc/<pid>/cgroup) up to the root of
// root and returns the smallest numeric memory.max, or false when none is set.
func tightestLimit(root, rel string) (cgLimit, bool) {
	best, ok := cgLimit{}, false
	dir := filepath.Join(root, rel)
	for {
		if v, isNum := readMemoryMax(filepath.Join(dir, "memory.max")); isNum && (!ok || v < best.limit) {
			best, ok = cgLimit{dir: dir, limit: v}, true
		}
		if dir == root || len(dir) <= len(root) {
			return best, ok
		}
		dir = filepath.Dir(dir)
	}
}

// readMemoryMax reads a memory.max file; "max" (no limit) and errors report false.
func readMemoryMax(path string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	return v, err == nil && v > 0
}

// avail is the limit minus the cgroup's working set; false when unreadable.
func (c cgLimit) avail() (uint64, bool) {
	cur, ok := readUintFile(filepath.Join(c.dir, "memory.current"))
	if !ok {
		return 0, false
	}
	if raw, err := os.ReadFile(filepath.Join(c.dir, "memory.stat")); err == nil {
		if inactive, ok := statField(raw, "inactive_file"); ok {
			cur -= min(inactive, cur)
		}
	}
	return c.limit - min(cur, c.limit), true
}

// statField reads "key value" from a memory.stat / vmstat style file.
func statField(raw []byte, key string) (uint64, bool) {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if ok && k == key {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

func readUintFile(path string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	return v, err == nil
}

// swapHeadroomMin: below this much free swap a host has, in effect, no swap left to
// page out to.
const swapHeadroomMin = 256 << 20

// noSwapHeadroom reports a Linux host with no swap, or with its swap all but used up.
// There the kernel has nothing to page out to, and running out of memory brings the OOM
// killer at once: the watchdog then treats available memory as under a hard limit.
func noSwapHeadroom(total, free uint64) bool {
	return total == 0 || free < swapHeadroomMin
}
