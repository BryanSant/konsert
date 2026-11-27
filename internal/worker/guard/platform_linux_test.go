//go:build linux

package guard

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestSlicePropsNeverHoard: the shared slice gets a soft ceiling and fork-bomb guard,
// never a guarantee (MemoryMin/MemoryLow), a hard memory cap, or a CPU quota unless a
// throttle asks for one.
func TestSlicePropsNeverHoard(t *testing.T) {
	b := NewBaseline(32<<30, true, "bare_metal")
	strict := sliceProps(ModeAdaptiveStrict, b, throttleState{}, 8)
	for _, p := range strict {
		for _, bad := range []string{"MemoryMin", "MemoryLow"} {
			if strings.HasPrefix(p, bad) {
				t.Fatalf("hoarding property %q in %v", p, strict)
			}
		}
	}
	want := []string{"MemoryMax=infinity", "CPUWeight=100", "CPUQuota=", "TasksMax=16384",
		"MemoryHigh=" + itoa(32<<30-1536<<20), "MemorySwapMax=" + itoa(2<<30)}
	for _, w := range want {
		if !slices.Contains(strict, w) {
			t.Fatalf("missing %q in %v", w, strict)
		}
	}
	adaptive := sliceProps(ModeAdaptive, b, throttleState{}, 8)
	if !slices.Contains(adaptive, "MemoryHigh=infinity") || !slices.Contains(adaptive, "MemorySwapMax=infinity") {
		t.Fatalf("adaptive must not set memory watermarks: %v", adaptive)
	}
	thr := sliceProps(ModeAdaptiveStrict, b, throttleState{on: true, cpuPct: 50}, 8)
	if !slices.Contains(thr, "CPUWeight=20") || !slices.Contains(thr, "CPUQuota=400%") {
		t.Fatalf("throttle props: %v", thr)
	}
}

func itoa(v uint64) string { return strconv.FormatUint(v, 10) }

func TestProcParsers(t *testing.T) {
	if v, ok := meminfoField([]byte("MemTotal:  100 kB\nMemAvailable:   2048 kB\n"), "MemAvailable:"); !ok || v != 2048<<10 {
		t.Fatalf("meminfo = %d %v", v, ok)
	}
	psi := []byte("some avg10=1.00 avg60=0.50 avg300=0.10 total=1\nfull avg10=23.45 avg60=1.00 avg300=0.00 total=2\n")
	if v, ok := psiFullAvg10(psi); !ok || v != 23.45 {
		t.Fatalf("psi = %v %v", v, ok)
	}
	if p := cgroupV2Path([]byte("0::/user.slice/user-1000.slice/user@1000.service/syre.slice/syre-workers.slice/syre-chat-1-2.scope\n")); !strings.HasSuffix(p, "syre-chat-1-2.scope") {
		t.Fatalf("cgroup path = %q", p)
	}
}
