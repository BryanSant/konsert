//go:build linux

package shells

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"time"
)

// clockTicks is USER_HZ, 100 on every Linux syred runs on.
const clockTicks = 100

// procSource reads /proc: no process spawned per tick.
type procSource struct {
	root string
	boot time.Time
}

// OSSource returns the process table of this machine.
func OSSource() Source { return &procSource{root: "/proc"} }

func (s *procSource) bootTime() time.Time {
	if !s.boot.IsZero() {
		return s.boot
	}
	raw, err := os.ReadFile(s.root + "/stat")
	if err != nil {
		return time.Time{}
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				s.boot = time.Unix(n, 0)
			}
		}
	}
	return s.boot
}

func (s *procSource) Snapshot() ([]Proc, error) {
	ents, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	boot := s.bootTime()
	out := make([]Proc, 0, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := os.ReadFile(s.root + "/" + e.Name() + "/stat")
		if err != nil {
			continue // exited meanwhile
		}
		if p, ok := parseStat(pid, raw, boot); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// parseStat parses /proc/<pid>/stat: "pid (comm) state ppid pgrp session tty tpgid
// flags minflt cminflt majflt cmajflt utime stime cutime cstime priority nice
// num_threads itrealvalue starttime …". comm may hold spaces and parentheses.
func parseStat(pid int, raw []byte, boot time.Time) (Proc, bool) {
	open := bytes.IndexByte(raw, '(')
	end := bytes.LastIndexByte(raw, ')')
	if open < 0 || end < open {
		return Proc{}, false
	}
	p := Proc{PID: pid, Name: string(raw[open+1 : end])}
	f := strings.Fields(string(raw[end+1:]))
	if len(f) < 20 {
		return Proc{}, false
	}
	p.Zombie = f[0] == "Z" || f[0] == "X"
	p.PPID, _ = strconv.Atoi(f[1])
	p.PGID, _ = strconv.Atoi(f[2])
	if tp, err := strconv.Atoi(f[5]); err == nil && tp > 0 {
		p.TPGID = tp
	}
	ut, _ := strconv.ParseInt(f[11], 10, 64)
	st, _ := strconv.ParseInt(f[12], 10, 64)
	p.CPU = time.Duration(ut+st) * time.Second / clockTicks
	p.CPUKnown = true
	if start, err := strconv.ParseInt(f[19], 10, 64); err == nil && !boot.IsZero() {
		p.Start = boot.Add(time.Duration(start) * time.Second / clockTicks)
	}
	return p, true
}

func (s *procSource) Details(*Proc) {}

func (s *procSource) Argv(pid int) []string {
	raw, err := os.ReadFile(s.root + "/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(raw) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
}
