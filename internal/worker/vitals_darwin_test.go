//go:build darwin

package worker

import (
	"bufio"
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/protocol"
)

// TestVitalsDarwinMatchesVMStat compares the sysctl memory reading with what konsertd
// computed from vm_stat before (active + wired + compressor pages): they should agree
// within a few percent of RAM.
func TestVitalsDarwinMatchesVMStat(t *testing.T) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		t.Skip("vm_stat:", err)
	}
	s := newVitalsSampler()
	var m protocol.WorkerMetrics
	s.sample(&m)
	pageSize := uint64(16384)
	var active, wired, compressed uint64
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		_, rhs, _ := strings.Cut(line, ":")
		v, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rhs), "."), 10, 64)
		switch {
		case strings.Contains(line, "page size of"):
			for _, f := range strings.Fields(line) {
				if n, err := strconv.ParseUint(f, 10, 64); err == nil && n >= 4096 {
					pageSize = n
				}
			}
		case strings.HasPrefix(line, "Pages active:"):
			active = v
		case strings.HasPrefix(line, "Pages wired down:"):
			wired = v
		case strings.HasPrefix(line, "Pages occupied by compressor:"):
			compressed = v
		}
	}
	vmstatMB := (active + wired + compressed) * pageSize >> 20
	diff := int64(m.MemUsedMB) - int64(vmstatMB)
	t.Logf("sysctl used %d MB, vm_stat used %d MB (diff %d MB of %d MB)", m.MemUsedMB, vmstatMB, diff, m.MemTotalMB)
	if diff < 0 {
		diff = -diff
	}
	if uint64(diff) > m.MemTotalMB/10 {
		t.Fatalf("sysctl and vm_stat disagree by %d MB (> 10%% of RAM)", diff)
	}
}
