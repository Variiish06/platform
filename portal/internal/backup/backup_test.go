package backup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fullStatus = `{
  "target": "drive",
  "running": "",
  "last_run": {"started": 1762900000, "finished": 1762900135, "status": "failed", "duration_s": 135.5,
               "bytes_added": 1048576, "error": "rclone: 403 <b>quota</b>"},
  "last_success": 1762813600,
  "snapshot_count": 12,
  "repo_size_bytes": 5368709120,
  "snapshots": [{"id": "a1b2c3d4", "time": 1762813600}, {"id": "../../etc", "time": 1}, {"id": "ABCDEF12"},
                {"id": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "time": 1762727200}],
  "last_check": {"time": 1762500000, "ok": true},
  "last_restore_test": {"time": 1762400000, "ok": false, "files_checked": 321, "detail": "hash mismatch"},
  "last_staging_restore": {"time": 1762600000, "ok": true, "snapshot": "a1b2c3d4",
                           "path": "/restore-staging/20261112T030000Z-a1b2c3d4", "detail": ""},
  "some_future_field": {"nested": [1, 2, 3]}
}`

func TestParseStatusFull(t *testing.T) {
	st, err := ParseStatus([]byte(fullStatus))
	if err != nil {
		t.Fatal(err)
	}
	if st.Target != "drive" || st.Running != "" || st.SnapshotCount != 12 || st.RepoSizeBytes != 5368709120 {
		t.Fatalf("top level: %+v", st)
	}
	r := st.LastRun
	if r == nil || r.Status != "failed" || r.Duration != 135500*time.Millisecond || r.BytesAdded != 1<<20 ||
		r.Error != "rclone: 403 <b>quota</b>" || r.Finished.Unix() != 1762900135 {
		t.Fatalf("last_run: %+v", r)
	}
	// Invalid ids (path, uppercase) are dropped, valid ones kept in order.
	if len(st.Snapshots) != 2 || st.Snapshots[0].ID != "a1b2c3d4" || len(st.Snapshots[1].ID) != 64 {
		t.Fatalf("snapshots: %+v", st.Snapshots)
	}
	if st.LastCheck == nil || st.LastCheck.OK == nil || !*st.LastCheck.OK {
		t.Fatalf("check: %+v", st.LastCheck)
	}
	if rt := st.RestoreTest; rt == nil || *rt.OK || rt.FilesChecked != 321 || rt.Detail != "hash mismatch" {
		t.Fatalf("restore test: %+v", rt)
	}
	if sr := st.StagingRestore; sr == nil || !*sr.OK || sr.Snapshot != "a1b2c3d4" || !strings.HasPrefix(sr.Path, "/restore-staging/") {
		t.Fatalf("staging: %+v", sr)
	}
}

func TestParseStatusLenient(t *testing.T) {
	// Wrong types are ignored field by field; the rest still parses.
	st, err := ParseStatus([]byte(`{"target": 5, "running": "backup", "snapshot_count": "many",
		"repo_size_bytes": -3, "last_success": "yesterday", "last_run": [], "snapshots": {"x":1},
		"last_check": {"ok": "yes"}, "target_extra": null}`))
	if err != nil {
		t.Fatal(err)
	}
	if st.Target != "" || st.Running != "backup" || st.SnapshotCount != -1 || st.RepoSizeBytes != -1 ||
		!st.LastSuccess.IsZero() || st.LastRun != nil || st.Snapshots != nil || st.LastCheck.OK != nil {
		t.Fatalf("%+v", st)
	}
	// Long strings are capped.
	st, _ = ParseStatus([]byte(`{"last_run": {"error": "` + strings.Repeat("x", 5000) + `"}}`))
	if n := len([]rune(st.LastRun.Error)); n != 501 {
		t.Fatalf("error not capped: %d runes", n)
	}
	// An empty object is valid: everything unknown.
	if st, err := ParseStatus([]byte(`{}`)); err != nil || st.SnapshotCount != -1 {
		t.Fatalf("empty: %v %+v", err, st)
	}
	for _, bad := range []string{``, `nope`, `[]`, `null`, `{"a":`} {
		if _, err := ParseStatus([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestReadStatusFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "status.json")
	if _, err := ReadStatus(p); !errors.Is(err, ErrNoStatus) {
		t.Fatalf("missing: %v", err)
	}
	os.WriteFile(p, []byte(fullStatus), 0o644)
	if st, err := ReadStatus(p); err != nil || st.Target != "drive" {
		t.Fatalf("ok file: %v", err)
	}
	os.WriteFile(p, []byte("{not json"), 0o644)
	if _, err := ReadStatus(p); err == nil || errors.Is(err, ErrNoStatus) {
		t.Fatalf("malformed: %v", err)
	}
	os.WriteFile(p, []byte(`{"target":"`+strings.Repeat("a", MaxStatusBytes)+`"}`), 0o644)
	if _, err := ReadStatus(p); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("oversized: %v", err)
	}
}

func TestRequestValidateEncode(t *testing.T) {
	good := map[Request]string{
		{Action: "backup"}:                                     `{"action":"backup"}`,
		{Action: "restore-staging", Snapshot: "latest"}:        `{"action":"restore-staging","snapshot":"latest"}`,
		{Action: "restore-staging", Snapshot: "a1b2c3d4"}:      `{"action":"restore-staging","snapshot":"a1b2c3d4"}`,
		{Action: "restore-staging", Snapshot: strings.Repeat("f", 64)}: `{"action":"restore-staging","snapshot":"` + strings.Repeat("f", 64) + `"}`,
	}
	for r, want := range good {
		b, err := r.Encode()
		if err != nil || string(b) != want {
			t.Errorf("%+v: %s %v", r, b, err)
		}
	}
	for _, r := range []Request{
		{Action: "restore"}, {Action: ""}, {Action: "backup", Snapshot: "latest"},
		{Action: "restore-staging"}, {Action: "restore-staging", Snapshot: "a1b2c3d"},
		{Action: "restore-staging", Snapshot: "A1B2C3D4"}, {Action: "restore-staging", Snapshot: "../../x"},
		{Action: "restore-staging", Snapshot: strings.Repeat("a", 65)}, {Action: "restore-staging", Snapshot: "latest\n"},
	} {
		if _, err := r.Encode(); err == nil {
			t.Errorf("%+v accepted", r)
		}
	}
}

func TestWriteRequest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "requests") // does not exist yet
	if pending, _ := Pending(dir); pending {
		t.Fatal("missing dir reported pending")
	}
	now := time.Unix(1762900000, 123456789)
	path, err := WriteRequest(dir, Request{Action: "restore-staging", Snapshot: "latest"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "1762900000123456789.json" || filepath.Dir(path) != dir {
		t.Fatalf("name %s", path)
	}
	b, _ := os.ReadFile(path)
	if string(b) != `{"action":"restore-staging","snapshot":"latest"}` {
		t.Fatalf("content %q", b)
	}
	fi, _ := os.Stat(path)
	di, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o644 || di.Mode().Perm() != 0o755 {
		t.Fatalf("modes file %v dir %v", fi.Mode(), di.Mode())
	}
	// Exactly one file: no temp file left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("%d entries in requests dir", len(entries))
	}
	if pending, at := Pending(dir); !pending || !at.Equal(now) {
		t.Fatalf("pending=%v at=%v", pending, at)
	}
	// Invalid requests write nothing.
	if _, err := WriteRequest(dir, Request{Action: "restore-staging", Snapshot: "x"}, now.Add(1)); err == nil {
		t.Fatal("invalid request written")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatal("invalid request left a file")
	}
	// Temp files (no .json suffix) are not "pending".
	os.Remove(path)
	os.WriteFile(filepath.Join(dir, ".portal-request-1.tmp"), []byte("{}"), 0o644)
	if pending, _ := Pending(dir); pending {
		t.Fatal("temp file counted as pending")
	}
}
