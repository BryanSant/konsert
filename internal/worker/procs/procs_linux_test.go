package procs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseStat(t *testing.T) {
	boot := time.Unix(1_000_000, 0)
	// comm with a space and a parenthesis; utime 150, stime 50, starttime 12345, rss 300.
	line := "4242 (my (odd) proc) S 1 4242 4242 0 -1 4194560 100 0 0 0 150 50 0 0 20 0 3 0 12345 123456789 300 18446744073709551615"
	p, ok := parseStat(4242, []byte(line), boot, 4096)
	if !ok {
		t.Fatal("not parsed")
	}
	if p.Name != "my (odd) proc" || p.PPID != 1 || p.CPU != 2*time.Second || p.RSS != 300*4096 ||
		!p.Start.Equal(boot.Add(123450*time.Millisecond)) {
		t.Fatalf("parsed %+v", p)
	}
	if _, ok := parseStat(1, []byte("1 (short) S 0"), boot, 4096); ok {
		t.Fatal("a truncated line parsed")
	}
}

func TestReadProcSkipsVanished(t *testing.T) {
	root := t.TempDir()
	write := func(rel, s string) {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755)
		if err := os.WriteFile(filepath.Join(root, rel), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("7/stat", "7 (a) S 1 7 7 0 -1 0 0 0 0 0 1 1 0 0 20 0 1 0 5 1 2 3")
	_ = os.MkdirAll(filepath.Join(root, "8"), 0o755) // exited: no stat
	write("self/stat", "not a pid dir")
	list, err := readProc(root, time.Unix(0, 0), 4096)
	if err != nil || len(list) != 1 || list[0].PID != 7 || list[0].RSS != 2*4096 {
		t.Fatalf("list = %+v, %v", list, err)
	}
}
