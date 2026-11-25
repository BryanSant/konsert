//go:build linux

package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BryanSant/syre/internal/protocol"
)

func TestVitalsLinuxFromProc(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("loadavg", "1.50 1.20 0.90 2/345 6789\n")
	write("meminfo", "MemTotal:       16000000 kB\nMemFree:         1000000 kB\nMemAvailable:    6000000 kB\nSwapTotal:       2048000 kB\nSwapFree:        1024000 kB\n")
	// user nice system idle iowait irq softirq steal guest guest_nice
	write("stat", "cpu  100 0 100 700 100 0 0 0 50 0\ncpu0 1 2 3 4\nintr 12345\n")
	s := &vitalsSampler{cores: 4, buf: make([]byte, 4096), procRoot: root}
	var m protocol.WorkerMetrics
	s.sample(&m)
	// No previous CPU sample yet: load / cores.
	if m.CPUPercent != 37.5 || m.LoadAvg != 1.5 {
		t.Fatalf("first sample cpu %v load %v", m.CPUPercent, m.LoadAvg)
	}
	if m.MemTotalMB != 15625 || m.MemUsedMB != 9765 || m.SwapTotalMB != 2000 || m.SwapUsedMB != 1000 {
		t.Fatalf("memory %+v", m)
	}
	// +300 busy (user 200, system 100) and +100 idle: 75 % busy; guest is not counted twice.
	write("stat", "cpu  300 0 200 800 100 0 0 0 150 0\n")
	m = protocol.WorkerMetrics{}
	s.sample(&m)
	if m.CPUPercent != 75 {
		t.Fatalf("cpu %v, want 75", m.CPUPercent)
	}
	// A counter reset (or garbage) falls back to load / cores.
	write("stat", "cpu  1 0 1 1 0 0 0 0\n")
	m = protocol.WorkerMetrics{}
	s.sample(&m)
	if m.CPUPercent != 37.5 {
		t.Fatalf("after reset cpu %v", m.CPUPercent)
	}
}
