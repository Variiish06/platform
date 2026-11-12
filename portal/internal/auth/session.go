package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

// Session lifetimes.
const (
	// SessionTTL is the sliding window: a session dies after this long
	// without any request. Every request pushes the deadline out again.
	SessionTTL = 30 * 24 * time.Hour
	// touchInterval limits how often a session row is rewritten. Without
	// it every proxied request (each image, each API call) would cost a
	// database write.
	touchInterval = 5 * time.Minute
)

// Session is one logged-in browser.
type Session struct {
	CreatedAt time.Time
	LastSeen  time.Time
	ExpiresAt time.Time
	IP        string
	UserAgent string
}

// SessionStore keeps sessions in SQLite.
//
// This is the "stateful session" design: the cookie holds only a random
// ID and the server looks it up on every request. Compared with a JWT
// (where the cookie itself carries signed claims) it costs one lookup per
// request, but logout and "revoke everything" take effect immediately.
type SessionStore struct {
	db  *sql.DB
	ttl time.Duration
	now func() time.Time // replaceable clock, so tests can travel in time
}

// NewSessionStore returns a store with the default 30-day sliding expiry.
func NewSessionStore(db *sql.DB) *SessionStore {
	return &SessionStore{db: db, ttl: SessionTTL, now: time.Now}
}

// hashToken returns what is stored in the database for a cookie value.
// Storing only the hash means a stolen database (or backup) does not
// contain usable session cookies. A plain SHA-256 is enough here because
// the token is 256 bits of randomness; there is nothing to brute-force.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Create starts a session and returns the secret token for the cookie.
func (s *SessionStore) Create(ip, userAgent string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	if len(userAgent) > 300 {
		userAgent = userAgent[:300]
	}
	_, err := s.db.Exec(`INSERT INTO sessions(token_hash, created_at, last_seen, expires_at, ip, user_agent)
		VALUES(?, ?, ?, ?, ?, ?)`,
		hashToken(token), now.Unix(), now.Unix(), now.Add(s.ttl).Unix(), ip, userAgent)
	if err != nil {
		return "", err
	}
	return token, nil
}

// Validate looks up a cookie value. It returns ok=false for unknown or
// expired sessions. A valid session has its expiry slid forward.
func (s *SessionStore) Validate(token string) (Session, bool) {
	if token == "" || len(token) > 128 {
		return Session{}, false
	}
	h := hashToken(token)
	var created, seen, expires int64
	var sess Session
	err := s.db.QueryRow(`SELECT created_at, last_seen, expires_at, ip, user_agent
		FROM sessions WHERE token_hash = ?`, h).Scan(&created, &seen, &expires, &sess.IP, &sess.UserAgent)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			// A database error must never let a request through.
			return Session{}, false
		}
		return Session{}, false
	}
	now := s.now()
	if now.Unix() >= expires {
		// Expired: remove it now rather than waiting for the hourly purge.
		s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, h)
		return Session{}, false
	}
	sess.CreatedAt = time.Unix(created, 0)
	sess.LastSeen = time.Unix(seen, 0)
	sess.ExpiresAt = time.Unix(expires, 0)

	// Sliding expiry: this request proves the session is still in use.
	if now.Sub(sess.LastSeen) >= touchInterval {
		sess.LastSeen = now
		sess.ExpiresAt = now.Add(s.ttl)
		s.db.Exec(`UPDATE sessions SET last_seen = ?, expires_at = ? WHERE token_hash = ?`,
			now.Unix(), sess.ExpiresAt.Unix(), h)
	}
	return sess, true
}

// Delete ends one session (logout).
func (s *SessionStore) Delete(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

// DeleteAll ends every session, e.g. after a password change.
func (s *SessionStore) DeleteAll() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sessions`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeExpired removes sessions whose deadline has passed.
func (s *SessionStore) PurgeExpired() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, s.now().Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// List returns all live sessions, most recently used first.
func (s *SessionStore) List() ([]Session, error) {
	rows, err := s.db.Query(`SELECT created_at, last_seen, expires_at, ip, user_agent
		FROM sessions WHERE expires_at > ? ORDER BY last_seen DESC`, s.now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var c, l, e int64
		var x Session
		if err := rows.Scan(&c, &l, &e, &x.IP, &x.UserAgent); err != nil {
			return nil, err
		}
		x.CreatedAt, x.LastSeen, x.ExpiresAt = time.Unix(c, 0), time.Unix(l, 0), time.Unix(e, 0)
		out = append(out, x)
	}
	return out, rows.Err()
}

// CountActive returns the number of live sessions (for /metrics).
func (s *SessionStore) CountActive() int {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE expires_at > ?`, s.now().Unix()).Scan(&n)
	return n
}
