package auth

import (
	"sync"
	"time"
)

// Limiter slows down password guessing with exponential backoff.
//
// Each client (keyed by IP address) gets a few free failures, so a typo
// costs nothing. After that, every further failure locks that client out
// for twice as long as the previous one:
//
//	failures:  1  2  3   4   5   6    7 ...
//	lockout:   -  -  -   2s  4s  8s  16s ...  capped at 15 minutes
//
// A person who mistypes barely notices; a script trying a wordlist gets
// about four guesses an hour once it hits the cap.
type Limiter struct {
	Free     int           // failures allowed before any lockout
	Base     time.Duration // first lockout
	Max      time.Duration // longest lockout
	ForgetIn time.Duration // failures are forgotten after this long without a new one

	mu      sync.Mutex
	clients map[string]*clientState
	now     func() time.Time
}

type clientState struct {
	failures    int
	lockedUntil time.Time
	lastFailure time.Time
}

// NewLimiter returns a limiter with the default policy.
func NewLimiter() *Limiter {
	return &Limiter{
		Free: 3, Base: 2 * time.Second, Max: 15 * time.Minute, ForgetIn: time.Hour,
		clients: make(map[string]*clientState), now: time.Now,
	}
}

// Allow reports whether key may attempt a login right now. If not, wait
// is how long it has to wait. Call it BEFORE checking the password, so a
// locked-out client cannot test guesses at all.
func (l *Limiter) Allow(key string) (ok bool, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.clients[key]
	if c == nil {
		return true, 0
	}
	now := l.now()
	if now.Sub(c.lastFailure) >= l.ForgetIn {
		delete(l.clients, key)
		return true, 0
	}
	if now.Before(c.lockedUntil) {
		return false, c.lockedUntil.Sub(now)
	}
	return true, 0
}

// Fail records a failed attempt and returns the lockout that now applies
// (zero while the client still has free attempts).
func (l *Limiter) Fail(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	c := l.clients[key]
	if c == nil || now.Sub(c.lastFailure) >= l.ForgetIn {
		c = &clientState{}
		l.clients[key] = c
	}
	c.failures++
	c.lastFailure = now
	over := c.failures - l.Free
	if over <= 0 {
		return 0
	}
	// Base * 2^(over-1), computed by doubling so it can never overflow.
	d := l.Base
	for i := 1; i < over && d < l.Max; i++ {
		d *= 2
	}
	if d > l.Max {
		d = l.Max
	}
	c.lockedUntil = now.Add(d)
	return d
}

// Success clears the client's failures after a correct login.
func (l *Limiter) Success(key string) {
	l.mu.Lock()
	delete(l.clients, key)
	l.mu.Unlock()
}

// Cleanup drops clients whose failures are old enough to forget. Without
// it the map would grow forever. Returns how many remain.
func (l *Limiter) Cleanup() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, c := range l.clients {
		if now.Sub(c.lastFailure) >= l.ForgetIn {
			delete(l.clients, k)
		}
	}
	return len(l.clients)
}
