package guard

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The tightest memory.max on the way up wins; "max" levels don't count.
func TestTightestLimit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "memory.max"), "10737418240\n") // the container: 10 GiB
	writeFile(t, filepath.Join(root, "a", "memory.max"), "max\n")
	writeFile(t, filepath.Join(root, "a", "b", "memory.max"), "21474836480\n") // looser below
	l, ok := tightestLimit(root, "/a/b")
	if !ok || l.limit != 10<<30 || l.dir != root {
		t.Fatalf("got %+v %v, want the 10 GiB root", l, ok)
	}
	if _, ok := tightestLimit(t.TempDir(), "/"); ok {
		t.Fatal("a limit without any memory.max")
	}
}

// Available is the limit less the working set: inactive file cache doesn't count.
func TestCgroupAvail(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "memory.current"), "6442450944\n") // 6 GiB charged
	writeFile(t, filepath.Join(dir, "memory.stat"), "anon 4294967296\nfile 2147483648\ninactive_file 1073741824\nactive_file 1073741824\n")
	v, ok := cgLimit{dir: dir, limit: 10 << 30}.avail()
	if !ok || v != 5<<30 {
		t.Fatalf("avail %d MiB %v, want 5120", v>>20, ok)
	}
	writeFile(t, filepath.Join(dir, "memory.current"), "12884901888\n") // over the limit
	if v, _ := (cgLimit{dir: dir, limit: 10 << 30}).avail(); v != 0 {
		t.Fatalf("avail %d over the limit, want 0", v)
	}
}

func TestNoSwapHeadroom(t *testing.T) {
	cases := []struct {
		total, free uint64
		want        bool
	}{
		{0, 0, true},                // no swap at all
		{2 << 30, 100 << 20, true},  // all but used up
		{2 << 30, 1 << 30, false},   // room to page out
		{8 << 30, 256 << 20, false}, // exactly at the floor
	}
	for _, c := range cases {
		if got := noSwapHeadroom(c.total, c.free); got != c.want {
			t.Errorf("noSwapHeadroom(%d MiB, %d MiB free) = %v, want %v", c.total>>20, c.free>>20, got, c.want)
		}
	}
}
