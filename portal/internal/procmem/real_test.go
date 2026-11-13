package procmem

import (
	"os"
	"testing"
	"time"
)

func TestRealHost(t *testing.T) {
	root := os.Getenv("PROCMEM_REAL")
	if root == "" {
		t.Skip()
	}
	s := &Scanner{Proc: root, SessionsDir: os.Getenv("PROCMEM_SESS"), UID: 1000}
	var best time.Duration = time.Hour
	var b Breakdown
	for range 10 {
		var err error
		b, err = s.Scan(nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		best = min(best, b.Took)
	}
	t.Logf("procs=%d best=%v last=%v", b.Procs, best, b.Took)
	for _, g := range b.Groups {
		t.Logf("%-12s %8.1f MiB n=%d", g.Key, float64(g.Bytes)/(1<<20), g.Count)
	}
	for _, ss := range b.Sessions {
		t.Logf("session %+v", ss)
	}
	for _, it := range b.Group(GUser).Items {
		t.Logf("user %s | %s | %.1f", it.Label, it.Sub, float64(it.Bytes)/(1<<20))
	}
}
