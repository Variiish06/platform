package auth

import (
	"testing"
	"time"
)

// Secret from RFC 6238 appendix B: ASCII "12345678901234567890" in base32.
const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

func TestTOTPMatchesRFC6238Vectors(t *testing.T) {
	// The RFC lists 8-digit codes; ours are the last 6 digits of each.
	for unix, want := range map[int64]string{
		59:          "287082",
		1111111109:  "081804",
		1111111111:  "050471",
		1234567890:  "005924",
		2000000000:  "279037",
		20000000000: "353130",
	} {
		got, err := totpCode(rfcSecret, unix/30)
		if err != nil || got != want {
			t.Errorf("t=%d: got %q (%v), want %q", unix, got, err, want)
		}
	}
}

func TestVerifyTOTPWindowAndReplay(t *testing.T) {
	now := time.Unix(1234567890, 0)
	code, _ := totpCode(rfcSecret, TOTPStep(now))

	step, ok := VerifyTOTP(rfcSecret, code, now, 0)
	if !ok {
		t.Fatal("current code rejected")
	}
	if _, ok := VerifyTOTP(rfcSecret, code, now, step); ok {
		t.Fatal("same code accepted twice (replay)")
	}
	if _, ok := VerifyTOTP(rfcSecret, code, now.Add(30*time.Second), 0); !ok {
		t.Fatal("code from the previous step rejected (no drift tolerance)")
	}
	if _, ok := VerifyTOTP(rfcSecret, code, now.Add(90*time.Second), 0); ok {
		t.Fatal("90-second-old code accepted")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef", "000000"} {
		if _, ok := VerifyTOTP(rfcSecret, bad, now, 0); ok {
			t.Errorf("bad code %q accepted", bad)
		}
	}
	if _, ok := VerifyTOTP(rfcSecret, " 005 924 ", now, 0); !ok {
		t.Fatal("code with spaces rejected")
	}
}

func TestNewTOTPSecret(t *testing.T) {
	a, _ := NewTOTPSecret()
	b, _ := NewTOTPSecret()
	if len(a) != 32 || a == b {
		t.Fatalf("bad secrets: %q %q", a, b)
	}
	if _, err := totpCode(a, 1); err != nil {
		t.Fatal(err)
	}
}
