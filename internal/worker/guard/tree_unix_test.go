//go:build darwin || linux

package guard

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestKillTakesOtherGroupsAndOrphans: an agent shell runs in its own process group
// (Claude Code does this), and a process it started outlives it, reparented to
// init/launchd. A session kill must still reach that process.
func TestKillTakesOtherGroupsAndOrphans(t *testing.T) {
	exe, _ := os.Executable()
	helper := func(role string) *exec.Cmd {
		c := exec.Command(exe, "-test.run", "^TestKillTakesOtherGroupsAndOrphans$")
		c.Env = append(os.Environ(), "SYRE_TREE_ROLE="+role)
		return c
	}
	switch os.Getenv("SYRE_TREE_ROLE") {
	case "root":
		mid := helper("mid")
		mid.Stdout = os.Stdout
		mid.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // like an agent's shell
		_ = mid.Start()
		time.Sleep(60 * time.Second)
		os.Exit(0)
	case "mid":
		leaf := helper("leaf")
		_ = leaf.Start()
		fmt.Println("leaf", leaf.Process.Pid)
		time.Sleep(1500 * time.Millisecond) // then exit: the leaf is orphaned
		os.Exit(0)
	case "leaf":
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}

	root := helper("root")
	out, _ := root.StdoutPipe()
	setGroup(root)
	if err := root.Start(); err != nil {
		t.Fatal(err)
	}
	g := newGuard(nativePlatform(), Config{Mode: ModePassthrough})
	s := g.Prepare(LaunchSpec{Binary: exe, Kind: "test"}).Attach(root.Process.Pid)
	leafPID := 0
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[0] == "leaf" {
			leafPID, _ = strconv.Atoi(f[1])
			break
		}
	}
	if leafPID == 0 {
		t.Fatal("no leaf reported")
	}
	defer syscall.Kill(leafPID, syscall.SIGKILL)
	g.plat.rss(s)                       // the watchdog's scans learn the tree while mid lives
	time.Sleep(2500 * time.Millisecond) // mid exits; the leaf is reparented
	if pgid, _ := syscall.Getpgid(leafPID); pgid == root.Process.Pid {
		t.Fatal("leaf is in the session's group; the test proves nothing")
	}
	if !s.Kill() {
		t.Fatal("kill failed")
	}
	_ = root.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(leafPID, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("orphan in another process group survived the session kill")
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.Release(root.ProcessState)
}
