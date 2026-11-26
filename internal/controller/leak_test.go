package controller

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
)

// goroutineEntry represents one parsed goroutine from runtime.Stack(..., true).
type goroutineEntry struct {
	header string
	stack  string
}

func parseGoroutines(buf []byte) []goroutineEntry {
	raw := strings.TrimSpace(string(buf))
	if raw == "" {
		return nil
	}
	blocks := strings.Split(raw, "\n\n")
	out := make([]goroutineEntry, 0, len(blocks))
	for _, b := range blocks {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		header, _, _ := strings.Cut(b, "\n")
		out = append(out, goroutineEntry{header: header, stack: b})
	}
	return out
}

// isAllowedGoroutine returns true for runtime/testing goroutines that are expected
// to still be alive when TestMain finishes.
func isAllowedGoroutine(g goroutineEntry) bool {
	if strings.Contains(g.stack, "checkLeakedGoroutines") ||
		strings.Contains(g.stack, "controller.TestMain") {
		return true
	}
	// Standard Go runtime / signal / net/http global transport idle connections.
	if strings.Contains(g.stack, "os/signal.") ||
		strings.Contains(g.stack, "runtime.ensureSigM") ||
		strings.Contains(g.stack, "net/http.(*persistConn).readLoop") ||
		strings.Contains(g.stack, "net/http.(*persistConn).writeLoop") {
		return true
	}
	return false
}

func snapshotLeakedGoroutines() (total int, leaked []goroutineEntry) {
	// Capture full stack dump, growing buffer as needed.
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, len(buf)*2)
	}
	all := parseGoroutines(buf)
	for _, g := range all {
		if !isAllowedGoroutine(g) {
			leaked = append(leaked, g)
		}
	}
	return len(all), leaked
}

// checkLeakedGoroutines waits briefly for any finishing goroutines to exit, then
// reports any remaining unexpected goroutines.
func checkLeakedGoroutines(maxWait time.Duration) (int, []goroutineEntry) {
	deadline := time.Now().Add(maxWait)
	for {
		runtime.GC()
		total, leaked := snapshotLeakedGoroutines()
		if len(leaked) == 0 || time.Now().After(deadline) {
			return total, leaked
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func summarizeLeaks(leaked []goroutineEntry) string {
	counts := map[string]int{}
	samples := map[string]string{}
	for _, g := range leaked {
		key := leakKey(g.stack)
		counts[key]++
		if _, ok := samples[key]; !ok {
			samples[key] = g.stack
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "  %4d x %s\n", counts[k], k)
	}
	sb.WriteString("\nSample stacks:\n")
	limit := min(len(keys), 15)
	for _, k := range keys[:limit] {
		fmt.Fprintf(&sb, "--- (%dx) %s ---\n%s\n\n", counts[k], k, samples[k])
	}
	return sb.String()
}

func leakKey(stack string) string {
	lines := strings.Split(stack, "\n")
	// Prefer the first frame in our module, or the "created by" line.
	var createdBy string
	var appFrame string
	for _, l := range lines[1:] {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "created by ") {
			if idx := strings.Index(t, " in goroutine "); idx > 0 {
				t = t[:idx]
			}
			createdBy = t
		}
		if appFrame == "" && strings.Contains(t, "github.com/BryanSant/syre/") && !strings.HasPrefix(t, "/") {
			if idx := strings.LastIndex(t, "("); idx > 0 {
				t = t[:idx]
			}
			appFrame = t
		}
	}
	if appFrame != "" && createdBy != "" {
		return appFrame + " [" + createdBy + "]"
	}
	if appFrame != "" {
		return appFrame
	}
	if createdBy != "" {
		return createdBy
	}
	if len(lines) > 1 {
		return strings.TrimSpace(lines[1])
	}
	return stack
}

func reportLeaksAtExit(code int) int {
	// Close idle connections on http.DefaultClient so transport goroutines aren't noise.
	http.DefaultClient.CloseIdleConnections()
	total, leaked := checkLeakedGoroutines(2 * time.Second)
	if len(leaked) > 0 {
		fmt.Fprintf(os.Stderr, "[goroutine-check] FAIL: %d leaked goroutines (total=%d)\n%s", len(leaked), total, summarizeLeaks(leaked))
		if os.Getenv("SYRE_LEAK_REPORT_ONLY") == "" && code == 0 {
			return 1
		}
	}
	return code
}
