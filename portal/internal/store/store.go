// Package store opens the SQLite database and owns its schema.
//
// SQLite is a library, not a server: the whole database is one file
// (data/portal.db) that this process reads and writes directly. For one
// user and a handful of sessions that is simpler, lighter and easier to
// back up than running Postgres.
package store

import (
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no C compiler needed)
)

// schema is applied on every start. "IF NOT EXISTS" makes it idempotent.
const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,   -- SHA-256 of the cookie value, never the value itself
	created_at INTEGER NOT NULL,   -- unix seconds
	last_seen  INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	ip         TEXT NOT NULL DEFAULT '',
	user_agent TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sessions_expires ON sessions(expires_at);
`

// Open opens (and creates if needed) the database at path.
func Open(path string) (*sql.DB, error) {
	// WAL mode lets readers and one writer work at the same time, which
	// matters because the CLI ("portal set-password") writes to the file
	// while the server has it open. busy_timeout makes a second writer
	// wait up to 5s instead of failing immediately.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2) // tiny pool: keeps memory low, plenty for one user
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return db, nil
}

// Setting keys.
const (
	KeyPasswordHash = "password_hash"
	KeyTOTPSecret   = "totp_secret"       // base32; present means 2FA is on
	KeyTOTPLastStep = "totp_last_step"    // last accepted time step (replay protection)
)

// GetSetting returns the value for key, or "" if it is not set.
func GetSetting(db *sql.DB, key string) (string, error) {
	var v string
	err := db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting stores value under key, replacing any previous value.
func SetSetting(db *sql.DB, key, value string) error {
	_, err := db.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// DeleteSetting removes key.
func DeleteSetting(db *sql.DB, key string) error {
	_, err := db.Exec(`DELETE FROM settings WHERE key = ?`, key)
	return err
}
