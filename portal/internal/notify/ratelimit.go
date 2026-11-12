package notify

import (
	"sync"
	"time"
)

// RateLimiter allows each key (a client IP) at most Limit requests per
// Window, counted in fixed windows. It is deliberately simple: its only
// job is to stop one misbehaving script from hammering ntfy through
// Portal. It lives in memory, so a Portal restart resets it.
type RateLimiter struct {
	Limit  int
	Window time.Duration

	mu      sync.Mutex
	clients map[string]*window
	now     func() time.Time
}

type window struct {
	start time.Time
	count int
}

// NewRateLimiter returns a limiter of limit requests per window.
func NewRateLimiter(limit int, per time.Duration) *RateLimiter {
	return &RateLimiter{Limit: limit, Window: per, clients: map[string]*window{}, now: time.Now}
}

// Allow counts one request for key and reports whether it may proceed.
// When it may not, retry is how long until the window resets.
func (l *RateLimiter) Allow(key string) (ok bool, retry time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.clients[key]
	if w == nil || now.Sub(w.start) >= l.Window {
		w = &window{start: now}
		l.clients[key] = w
	}
	if w.count >= l.Limit {
		return false, w.start.Add(l.Window).Sub(now)
	}
	w.count++
	return true, 0
}

// Cleanup forgets keys whose window has ended, so the map cannot grow
// forever. Returns how many remain.
func (l *RateLimiter) Cleanup() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, w := range l.clients {
		if now.Sub(w.start) >= l.Window {
			delete(l.clients, k)
		}
	}
	return len(l.clients)
}
