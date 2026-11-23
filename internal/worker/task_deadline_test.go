package worker

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestTaskDeadlinePauseKeepsRemainingTime(t *testing.T) {
	var fired atomic.Int64
	start := time.Now()
	d := newTaskDeadline(300*time.Millisecond, func() { fired.Store(time.Since(start).Milliseconds()) })
	time.Sleep(100 * time.Millisecond)
	d.pause()
	time.Sleep(400 * time.Millisecond) // past the original deadline
	if fired.Load() != 0 {
		t.Fatal("paused deadline fired")
	}
	d.resume()
	deadline := time.Now().Add(2 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ms := fired.Load(); ms < 550 || ms > 1500 {
		t.Fatalf("fired after %dms, want ~700ms (100 run + 400 paused + 200 left)", ms)
	}

	var stoppedFired atomic.Bool
	s := newTaskDeadline(50*time.Millisecond, func() { stoppedFired.Store(true) })
	s.stop()
	s.resume() // no effect after stop
	time.Sleep(150 * time.Millisecond)
	if stoppedFired.Load() {
		t.Fatal("stopped deadline fired")
	}
	var none *taskDeadline // no timeout: every method is a no-op
	none.pause()
	none.resume()
	none.stop()
}
