//go:build linux

package worker

import (
	"bytes"
	"os"
	"runtime"
	"strconv"

	"github.com/BryanSant/konsert/internal/protocol"
)

// vitalsSampler reads Linux vitals from /proc: loadavg, the first line of stat (CPU %
// as the busy share of the jiffies since the previous sample) and meminfo. Three small
// reads into one reused buffer; no subprocess.
type vitalsSampler struct {
	cores     float64
	buf       []byte
	prevBusy  uint64
	prevTotal uint64
	procRoot  string // "/proc"; tests point it elsewhere
}

func newVitalsSampler() *vitalsSampler {
	return &vitalsSampler{cores: float64(runtime.NumCPU()), buf: make([]byte, 4096), procRoot: "/proc"}
}

// read returns up to len(buf) bytes of a /proc file (valid until the next read).
func (s *vitalsSampler) read(name string) []byte {
	f, err := os.Open(s.procRoot + "/" + name)
	if err != nil {
		return nil
	}
	defer f.Close()
	n := 0
	for n < len(s.buf) {
		k, err := f.Read(s.buf[n:])
		n += k
		if err != nil || k == 0 {
			break
		}
	}
	return s.buf[:n]
}

func (s *vitalsSampler) sample(m *protocol.WorkerMetrics) {
	if raw := s.read("loadavg"); len(raw) > 0 {
		if f, ok := firstField(raw); ok {
			if la, err := strconv.ParseFloat(string(f), 64); err == nil {
				m.LoadAvg = la
			}
		}
	}
	m.CPUPercent = loadPercent(m.LoadAvg, s.cores)
	if raw := s.read("stat"); len(raw) > 0 {
		if busy, total, ok := parseProcStatCPU(raw); ok {
			if s.prevTotal > 0 && total > s.prevTotal && busy >= s.prevBusy {
				m.CPUPercent = min(100, float64(busy-s.prevBusy)/float64(total-s.prevTotal)*100)
			}
			s.prevBusy, s.prevTotal = busy, total
		}
	}
	if raw := s.read("meminfo"); len(raw) > 0 {
		parseMeminfo(raw, m)
	}
}

func firstField(b []byte) ([]byte, bool) {
	b = bytes.TrimLeft(b, " \t")
	if i := bytes.IndexAny(b, " \t\n"); i > 0 {
		return b[:i], true
	}
	return b, len(b) > 0
}

// parseProcStatCPU reads the aggregate "cpu" line of /proc/stat: total is user + nice +
// system + idle + iowait + irq + softirq + steal (guest time is already in user), busy is
// total less idle and iowait.
func parseProcStatCPU(raw []byte) (busy, total uint64, ok bool) {
	line, _, _ := bytes.Cut(raw, []byte{'\n'})
	if !bytes.HasPrefix(line, []byte("cpu ")) {
		return 0, 0, false
	}
	fields := bytes.Fields(line[4:])
	if len(fields) < 4 {
		return 0, 0, false
	}
	var idle uint64
	for i, f := range fields {
		if i >= 8 {
			break
		}
		v, err := strconv.ParseUint(string(f), 10, 64)
		if err != nil {
			return 0, 0, false
		}
		total += v
		if i == 3 || i == 4 { // idle, iowait
			idle += v
		}
	}
	return total - idle, total, true
}

// parseMeminfo fills memory and swap from /proc/meminfo (used = total - available).
func parseMeminfo(raw []byte, m *protocol.WorkerMetrics) {
	var totalKB, availKB, swapTotalKB, swapFreeKB uint64
	for len(raw) > 0 {
		var line []byte
		line, raw, _ = bytes.Cut(raw, []byte{'\n'})
		key, rest, ok := bytes.Cut(line, []byte{':'})
		if !ok {
			continue
		}
		var dst *uint64
		switch string(key) {
		case "MemTotal":
			dst = &totalKB
		case "MemAvailable":
			dst = &availKB
		case "SwapTotal":
			dst = &swapTotalKB
		case "SwapFree":
			dst = &swapFreeKB
		default:
			continue
		}
		if f, ok := firstField(rest); ok {
			*dst, _ = strconv.ParseUint(string(f), 10, 64)
		}
	}
	m.MemTotalMB = totalKB / 1024
	if totalKB >= availKB {
		m.MemUsedMB = (totalKB - availKB) / 1024
	}
	m.SwapTotalMB = swapTotalKB / 1024
	if swapTotalKB >= swapFreeKB {
		m.SwapUsedMB = (swapTotalKB - swapFreeKB) / 1024
	}
}
