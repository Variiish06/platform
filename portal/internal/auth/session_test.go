package auth

import (
	"path/filepath"
	"testing"
	"time"

	"portal/internal/store"
)

// newTestStore returns a store on a throwaway database with a fake clock.
func newTestStore(t *testing.T) (*SessionStore, *time.Time) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s := NewSessionStore(db)
	s.now = func() time.Time { return now }
	return s, &now
}

func TestSessionCreateValidateDelete(t *testing.T) {
	s, _ := newTestStore(t)
	tok, err := s.Create("100.64.0.1", "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 40 {
		t.Fatalf("token too short: %q", tok)
	}
	sess, ok := s.Validate(tok)
	if !ok || sess.IP != "100.64.0.1" {
		t.Fatalf("fresh session invalid: %+v ok=%v", sess, ok)
	}
	for _, bad := range []string{"", "nope", tok + "x", tok[:len(tok)-1]} {
		if _, ok := s.Validate(bad); ok {
			t.Errorf("Validate(%q) accepted", bad)
		}
	}
	if err := s.Delete(tok); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Validate(tok); ok {
		t.Fatal("session still valid after logout")
	}
}

func TestSessionTokenNotStoredInPlaintext(t *testing.T) {
	s, _ := newTestStore(t)
	tok, _ := s.Create("", "")
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = ?`, tok).Scan(&n)
	if n != 0 {
		t.Fatal("raw token found in the database")
	}
}

func TestSessionExpiresAfterIdleTTL(t *testing.T) {
	s, now := newTestStore(t)
	tok, _ := s.Create("", "")

	*now = now.Add(SessionTTL - time.Minute)
	if _, ok := s.Validate(tok); !ok {
		t.Fatal("session expired too early")
	}
	// The validation above slid the window, so jump a full TTL from here.
	*now = now.Add(SessionTTL + time.Second)
	if _, ok := s.Validate(tok); ok {
		t.Fatal("session still valid after 30 idle days")
	}
	if s.CountActive() != 0 {
		t.Fatal("expired session was not removed")
	}
}

func TestSessionSlidingExpiry(t *testing.T) {
	s, now := newTestStore(t)
	tok, _ := s.Create("", "")
	// Use it every 20 days for 100 days: far beyond the 30-day TTL, but
	// never idle for 30 days, so it must stay alive.
	for i := 0; i < 5; i++ {
		*now = now.Add(20 * 24 * time.Hour)
		sess, ok := s.Validate(tok)
		if !ok {
			t.Fatalf("session died on visit %d despite regular use", i+1)
		}
		if want := now.Add(SessionTTL); !sess.ExpiresAt.Equal(want) {
			t.Fatalf("expiry not slid: got %v want %v", sess.ExpiresAt, want)
		}
	}
}

func TestSessionTouchIsThrottled(t *testing.T) {
	s, now := newTestStore(t)
	tok, _ := s.Create("", "")
	first, _ := s.Validate(tok)
	*now = now.Add(touchInterval - time.Second)
	second, _ := s.Validate(tok)
	if !second.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatal("expiry rewritten before the touch interval elapsed")
	}
}

func TestPurgeAndDeleteAll(t *testing.T) {
	s, now := newTestStore(t)
	s.Create("", "")
	s.Create("", "")
	*now = now.Add(SessionTTL + time.Hour)
	keep, _ := s.Create("", "")
	if n, _ := s.PurgeExpired(); n != 2 {
		t.Fatalf("purged %d, want 2", n)
	}
	if _, ok := s.Validate(keep); !ok {
		t.Fatal("live session was purged")
	}
	if n, _ := s.DeleteAll(); n != 1 {
		t.Fatalf("DeleteAll removed %d, want 1", n)
	}
}
