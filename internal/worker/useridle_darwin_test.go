package worker

import (
	"context"
	"testing"
	"time"
)

func TestParseHIDIdle(t *testing.T) {
	out := []byte("    | |   \"HIDIdleTime\" = 88157875\n    | |   \"HIDIdleTimeOther\" = 1\n")
	if d, src, ok := parseHIDIdle(out); !ok || d != 88157875*time.Nanosecond || src != "hid" {
		t.Fatalf("got %v %s %v", d, src, ok)
	}
	if _, _, ok := parseHIDIdle([]byte("nothing")); ok {
		t.Fatal("parsed nothing")
	}
}

func TestPlatformUserIdleLive(t *testing.T) {
	d, src, ok := platformUserIdle(context.Background())
	if !ok || src != "hid" || d < 0 {
		t.Fatalf("idle = %v %s %v", d, src, ok)
	}
}
