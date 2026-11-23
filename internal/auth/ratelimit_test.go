package auth

import (
	"testing"
	"time"
)

func TestLoginLimiterBackoff(t *testing.T) {
	l := NewLoginLimiter()
	now := time.Unix(1_700_000_000, 0)
	l.Now = func() time.Time { return now }

	for i := 0; i < l.FreeAttempts; i++ {
		if ok, _ := l.Allow("1.2.3.4"); !ok {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
		if d := l.Failure("1.2.3.4"); d != 0 {
			t.Fatalf("free attempt %d should not lock, got %v", i+1, d)
		}
	}
	if d := l.Failure("1.2.3.4"); d != time.Second {
		t.Fatalf("first lockout = %v, want 1s", d)
	}
	if ok, wait := l.Allow("1.2.3.4"); ok || wait <= 0 {
		t.Fatal("expected lockout")
	}
	// Other clients are unaffected.
	if ok, _ := l.Allow("5.6.7.8"); !ok {
		t.Fatal("other IP must not be throttled")
	}

	now = now.Add(time.Second)
	if d := l.Failure("1.2.3.4"); d != 2*time.Second {
		t.Fatalf("second lockout = %v, want 2s", d)
	}
	for i := 0; i < 20; i++ {
		l.Failure("1.2.3.4")
	}
	now = now.Add(time.Hour)
	if d := l.Failure("1.2.3.4"); d != l.MaxDelay {
		t.Fatalf("lockout must cap at %v, got %v", l.MaxDelay, d)
	}

	l.Success("1.2.3.4")
	if ok, _ := l.Allow("1.2.3.4"); !ok {
		t.Fatal("success must clear the lockout")
	}
}
