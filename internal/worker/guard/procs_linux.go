//go:build linux

package guard

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// listProcs lists every process from /proc/<pid>/stat.
func listProcs() ([]procInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make([]procInfo, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp ... starttime(22nd): comm may contain spaces.
		i := bytes.LastIndexByte(stat, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(stat[i+1:]))
		if len(f) < 20 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		pgid, _ := strconv.Atoi(f[2])
		start, _ := strconv.ParseUint(f[19], 10, 64)
		out = append(out, procInfo{pid: pid, ppid: ppid, pgid: pgid, start: start})
	}
	return out, nil
}

// pidsRSS sums the resident memory of pids.
func pidsRSS(pids []int) uint64 {
	page := uint64(os.Getpagesize())
	var total uint64
	for _, pid := range pids {
		statm, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
		if err != nil {
			continue
		}
		if sf := strings.Fields(string(statm)); len(sf) >= 2 {
			if pages, err := strconv.ParseUint(sf[1], 10, 64); err == nil {
				total += pages * page
			}
		}
	}
	return total
}
