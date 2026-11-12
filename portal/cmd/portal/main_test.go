package main

import (
	"io"
	"path/filepath"
	"testing"
	"time"

	"portal/internal/auth"
	"portal/internal/store"
)

func TestCreateSessionCLI(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := auth.NewSessionStore(db)

	token, expires, err := createSession(st, []string{"--ttl", "10m", "--note", "test"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(expires); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("expiry %v from now, want about 10m", d)
	}
	sess, ok := st.Validate(token)
	if !ok || sess.IP != "cli" || sess.UserAgent != "portal sessions create: test" {
		t.Fatalf("minted session not usable: %+v ok=%v", sess, ok)
	}
	if d := time.Until(sess.ExpiresAt); d > 11*time.Minute {
		t.Fatalf("session stored with %v left, want <= 10m", d)
	}

	for _, bad := range [][]string{{"--ttl", "0s"}, {"--ttl", "25h"}, {"--ttl", "soon"}, {"extra"}, {"--nope"}} {
		if _, _, err := createSession(st, bad, io.Discard); err == nil {
			t.Errorf("createSession(%q) accepted", bad)
		}
	}
	if _, _, err := createSession(st, nil, io.Discard); err != nil {
		t.Errorf("defaults rejected: %v", err)
	}
}
