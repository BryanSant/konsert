//go:build linux

package procs

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// clockTicks is USER_HZ, 100 on every Linux syred runs on.
const clockTicks = 100

var (
	bootOnce sync.Once
	bootAt   time.Time
)

// bootTime is btime from /proc/stat: /proc/<pid>/stat gives start times relative to it.
func bootTime() time.Time {
	bootOnce.Do(func() {
		raw, err := os.ReadFile("/proc/stat")
		if err != nil {
			return
		}
		for line := range strings.SplitSeq(string(raw), "\n") {
			if v, ok := strings.CutPrefix(line, "btime "); ok {
				if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
					bootAt = time.Unix(n, 0)
				}
			}
		}
	})
	return bootAt
}

func osTable() ([]Proc, error) { return readProc("/proc", bootTime(), uint64(os.Getpagesize())) }

// readProc reads root/<pid>/stat for every process.
func readProc(root string, boot time.Time, pageSize uint64) ([]Proc, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := make([]Proc, 0, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := os.ReadFile(root + "/" + e.Name() + "/stat")
		if err != nil {
			continue // exited meanwhile
		}
		if p, ok := parseStat(pid, raw, boot, pageSize); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// parseStat parses /proc/<pid>/stat: "pid (comm) state ppid pgrp session tty_nr tpgid
// flags minflt cminflt majflt cmajflt utime stime cutime cstime priority nice
// num_threads itrealvalue starttime vsize rss …". comm may hold spaces and parentheses.
func parseStat(pid int, raw []byte, boot time.Time, pageSize uint64) (Proc, bool) {
	open := bytes.IndexByte(raw, '(')
	end := bytes.LastIndexByte(raw, ')')
	if open < 0 || end < open {
		return Proc{}, false
	}
	f := strings.Fields(string(raw[end+1:]))
	if len(f) < 22 {
		return Proc{}, false
	}
	p := Proc{PID: pid, Name: string(raw[open+1 : end])}
	p.PPID, _ = strconv.Atoi(f[1])
	if tty, err := strconv.ParseInt(f[4], 10, 64); err == nil && tty > 0 {
		p.TTY = uint64(tty)
	}
	ut, _ := strconv.ParseInt(f[11], 10, 64)
	st, _ := strconv.ParseInt(f[12], 10, 64)
	p.CPU = time.Duration(ut+st) * time.Second / clockTicks
	if start, err := strconv.ParseInt(f[19], 10, 64); err == nil && !boot.IsZero() {
		p.Start = boot.Add(time.Duration(start) * time.Second / clockTicks)
	}
	if pages, err := strconv.ParseInt(f[21], 10, 64); err == nil && pages > 0 {
		p.RSS = uint64(pages) * pageSize
	}
	return p, true
}
