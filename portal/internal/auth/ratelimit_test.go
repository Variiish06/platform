package auth

import (
	"testing"
	"time"
)

func newTestLimiter() (*Limiter, *time.Time) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewLimiter()
	l.now = func() time.Time { return now }
	return l, &now
}

func TestLimiterFreeAttempts(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("ip"); !ok {
			t.Fatalf("attempt %d blocked during free attempts", i+1)
		}
		if d := l.Fail("ip"); d != 0 {
			t.Fatalf("failure %d caused a lockout of %v", i+1, d)
		}
	}
	if ok, _ := l.Allow("ip"); !ok {
		t.Fatal("blocked right after the free attempts, before a 4th failure")
	}
}

func TestLimiterExponentialBackoff(t *testing.T) {
	l, now := newTestLimiter()
	for i := 0; i < 3; i++ {
		l.Fail("ip")
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second}
	for i, w := range want {
		if d := l.Fail("ip"); d != w {
			t.Fatalf("lockout %d = %v, want %v", i+1, d, w)
		}
		ok, wait := l.Allow("ip")
		if ok || wait != w {
			t.Fatalf("during lockout: ok=%v wait=%v, want blocked for %v", ok, wait, w)
		}
		*now = now.Add(w - time.Millisecond)
		if ok, _ := l.Allow("ip"); ok {
			t.Fatal("allowed 1ms before the lockout ended")
		}
		*now = now.Add(time.Millisecond)
		if ok, _ := l.Allow("ip"); !ok {
			t.Fatal("still blocked after the lockout ended")
		}
	}
}

func TestLimiterCapsAtMax(t *testing.T) {
	l, now := newTestLimiter()
	var d time.Duration
	for i := 0; i < 200; i++ { // far past the point where 2^n would overflow
		d = l.Fail("ip")
		*now = now.Add(d)
	}
	if d != l.Max {
		t.Fatalf("lockout = %v, want cap %v", d, l.Max)
	}
}

func TestLimiterKeysAreIndependent(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 6; i++ {
		l.Fail("attacker")
	}
	if ok, _ := l.Allow("attacker"); ok {
		t.Fatal("attacker not locked out")
	}
	if ok, _ := l.Allow("me"); !ok {
		t.Fatal("another client was locked out too")
	}
}

func TestLimiterSuccessResets(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 5; i++ {
		l.Fail("ip")
	}
	l.Success("ip")
	if ok, _ := l.Allow("ip"); !ok {
		t.Fatal("still locked after a successful login")
	}
	if d := l.Fail("ip"); d != 0 {
		t.Fatalf("failure count not reset: lockout %v", d)
	}
}

func TestLimiterForgetsOldFailures(t *testing.T) {
	l, now := newTestLimiter()
	for i := 0; i < 3; i++ {
		l.Fail("ip")
	}
	*now = now.Add(l.ForgetIn)
	if d := l.Fail("ip"); d != 0 {
		t.Fatalf("old failures still counted: lockout %v", d)
	}
	*now = now.Add(l.ForgetIn)
	if n := l.Cleanup(); n != 0 {
		t.Fatalf("Cleanup left %d entries", n)
	}
}
