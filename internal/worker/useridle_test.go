package worker

import (
	"context"
	"testing"
	"time"
)

func TestUserIdleCache(t *testing.T) {
	prev := readUserIdle
	t.Cleanup(func() { readUserIdle = prev })
	calls := 0
	readUserIdle = func(context.Context) (time.Duration, string, bool) { calls++; return time.Minute, "hid", true }
	var c userIdleCache
	d1, src, ok := c.get(context.Background())
	d2, _, _ := c.get(context.Background())
	if calls != 1 || !ok || src != "hid" || d1 < time.Minute || d2 < d1 {
		t.Fatalf("calls %d, %v then %v", calls, d1, d2)
	}
}
