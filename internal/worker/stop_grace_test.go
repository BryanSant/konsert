package worker

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

func TestNormalizeStopGraceS(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{-5, protocol.DefaultStopGraceS},
		{0, protocol.DefaultStopGraceS},
		{1, 1},
		{10, 10},
		{300, 300},
		{301, protocol.MaxStopGraceS},
		{9999, protocol.MaxStopGraceS},
	}
	for _, tc := range cases {
		if got := protocol.NormalizeStopGraceS(tc.in); got != tc.want {
			t.Errorf("NormalizeStopGraceS(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestTerminateWithGraceHelper(t *testing.T) {
	t.Run("immediate_exit_on_interrupt", func(t *testing.T) {
		exited := make(chan struct{})
		var interrupted, killed atomic.Bool

		start := time.Now()
		err := terminateWithGrace(
			5*time.Second,
			func() error {
				interrupted.Store(true)
				close(exited)
				return nil
			},
			func() (int, error) {
				<-exited
				return 0, nil
			},
			func() error {
				killed.Store(true)
				return nil
			},
		)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("terminateWithGrace: %v", err)
		}
		if !interrupted.Load() {
			t.Fatal("expected interrupt to be called")
		}
		if killed.Load() {
			t.Fatal("expected kill NOT to be called when process exits on interrupt")
		}
		if elapsed > 1*time.Second {
			t.Fatalf("expected immediate return, took %v", elapsed)
		}
	})

	t.Run("grace_honoured_then_kill", func(t *testing.T) {
		exited := make(chan struct{})
		var interrupted, killed atomic.Bool
		grace := 250 * time.Millisecond

		start := time.Now()
		err := terminateWithGrace(
			grace,
			func() error {
				interrupted.Store(true)
				return nil
			},
			func() (int, error) {
				<-exited
				return 137, nil
			},
			func() error {
				killed.Store(true)
				close(exited)
				return nil
			},
		)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("terminateWithGrace: %v", err)
		}
		if !interrupted.Load() {
			t.Fatal("expected interrupt to be called")
		}
		if !killed.Load() {
			t.Fatal("expected kill to be called after grace expired")
		}
		if elapsed < grace-30*time.Millisecond {
			t.Fatalf("kill fired before grace period expired: elapsed=%v, grace=%v", elapsed, grace)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("took too long after grace: elapsed=%v", elapsed)
		}
	})
}
