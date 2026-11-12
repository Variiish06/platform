package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP (RFC 6238) is the "6 digits from an authenticator app" second
// factor. The phone and the server share a secret. Both compute
// HMAC-SHA1(secret, number of 30-second steps since 1970) and show six
// digits of the result. Matching codes prove the phone holds the secret,
// without the secret ever being sent.

const (
	totpStep   = 30 // seconds per code
	totpDigits = 6
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random 160-bit secret in base32, the format
// authenticator apps expect.
func NewTOTPSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return b32.EncodeToString(raw), nil
}

// TOTPStep returns the 30-second step number that t falls in.
func TOTPStep(t time.Time) int64 { return t.Unix() / totpStep }

// totpCode computes the code for one step (RFC 4226 "dynamic truncation").
func totpCode(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", fmt.Errorf("auth: bad TOTP secret: %w", err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	// The last 4 bits pick which 4 bytes of the MAC to use.
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", bin%1_000_000), nil
}

// VerifyTOTP checks code against the secret at time t. It accepts the
// previous and next step too, to allow for clock drift and slow typing.
//
// lastStep is the step of the last code that was accepted. A code is only
// good once: anything at or before lastStep is refused, so someone who
// watches you type a code cannot reuse it within its 30 seconds. On
// success it returns the step to store as the new lastStep.
func VerifyTOTP(secret, code string, t time.Time, lastStep int64) (step int64, ok bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	now := TOTPStep(t)
	for _, s := range []int64{now, now - 1, now + 1} {
		if s <= lastStep {
			continue
		}
		want, err := totpCode(secret, s)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}

// TOTPURI returns the otpauth:// link that authenticator apps import
// (usually shown as a QR code).
func TOTPURI(secret, issuer, account string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", "6")
	v.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + v.Encode()
}
