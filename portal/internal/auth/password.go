// Package auth holds the security-sensitive pieces of Portal: password
// hashing, sessions, redirect validation, rate limiting and TOTP.
// Nothing here knows about HTTP handlers, which keeps it easy to test.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2Params are the cost settings for argon2id.
//
// A password hash is deliberately slow and memory-hungry. If the database
// ever leaks, an attacker must spend this much memory and time for every
// single guess, which makes GPU and ASIC cracking rigs far less effective
// than they are against fast hashes such as SHA-256.
type Argon2Params struct {
	Memory  uint32 // KiB of RAM per hash
	Time    uint32 // passes over that memory
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultParams follows OWASP's minimum recommendation for argon2id
// (19 MiB, 2 passes, 1 thread). It is on the small side on purpose: the
// whole platform has a 150 MB RAM budget, and each login allocates this much.
var DefaultParams = Argon2Params{Memory: 19 * 1024, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}

// hashMu lets only one hash run at a time, so a burst of login attempts
// cannot multiply the memory use (10 parallel logins would otherwise need
// 190 MiB and get the container killed).
var hashMu sync.Mutex

// HashPassword returns an encoded hash in the standard PHC string format:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
//
// The parameters and the random salt are stored inside the string, so
// VerifyPassword needs nothing else, and the parameters can be raised
// later without breaking existing hashes.
func HashPassword(password string, p Argon2Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hashMu.Lock()
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	hashMu.Unlock()
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// ErrBadHash means the stored hash string could not be parsed.
var ErrBadHash = errors.New("auth: malformed password hash")

// VerifyPassword reports whether password matches the encoded hash.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrBadHash
	}
	var p Argon2Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return false, ErrBadHash
	}
	// Refuse absurd parameters so a corrupted row cannot exhaust memory.
	if p.Memory > 1<<20 || p.Time > 16 || p.Threads == 0 {
		return false, ErrBadHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, ErrBadHash
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, ErrBadHash
	}
	hashMu.Lock()
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(want)))
	hashMu.Unlock()
	// Constant-time comparison: the time taken does not depend on how many
	// leading bytes match, so response timing leaks nothing about the hash.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
