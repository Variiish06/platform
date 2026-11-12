package auth

import (
	"strings"
	"testing"
)

// Small parameters keep the tests fast; verification reads the parameters
// from the hash string, so the code path is the same as in production.
var testParams = Argon2Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery staple", testParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("unexpected format: %s", h)
	}
	if strings.Contains(h, "correct horse") {
		t.Fatal("hash contains the plaintext")
	}
	if ok, err := VerifyPassword("correct horse battery staple", h); err != nil || !ok {
		t.Fatalf("right password rejected: ok=%v err=%v", ok, err)
	}
	if ok, _ := VerifyPassword("wrong", h); ok {
		t.Fatal("wrong password accepted")
	}
	if ok, _ := VerifyPassword("", h); ok {
		t.Fatal("empty password accepted")
	}
}

func TestHashUsesRandomSalt(t *testing.T) {
	a, _ := HashPassword("same", testParams)
	b, _ := HashPassword("same", testParams)
	if a == b {
		t.Fatal("two hashes of the same password are identical: salt is not random")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, h := range []string{
		"", "plaintext", "$argon2i$v=19$m=64,t=1,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=18$m=64,t=1,p=1$c2FsdA$aGFzaA", // wrong version
		"$argon2id$v=19$m=64,t=1,p=1$!!!$aGFzaA",    // bad base64
		"$argon2id$v=19$m=64,t=1,p=1$c2FsdA$",       // empty hash
		"$argon2id$v=19$m=99999999,t=1,p=1$c2FsdA$aGFzaA", // absurd memory
	} {
		if ok, err := VerifyPassword("x", h); ok || err == nil {
			t.Errorf("VerifyPassword(%q) = %v, %v; want false with an error", h, ok, err)
		}
	}
}

func TestDefaultParamsVerify(t *testing.T) {
	h, err := HashPassword("pw", DefaultParams)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := VerifyPassword("pw", h); !ok {
		t.Fatal("default params round trip failed")
	}
}
