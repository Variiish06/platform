// Package backup is Portal's side of the contract with the backup
// container (PLAN-2.md, Phase C). Portal never runs restic, never talks to
// Docker for backups and never touches the repository. It does exactly two
// things, both through files in the shared ./data/backup directory:
//
//   - READ  status.json, which the backup container rewrites (atomically)
//     whenever something changes: what is running, how the last run went,
//     repository size, snapshots...
//   - WRITE a request file requests/<unix-nanos>.json, which a watcher in
//     the backup container picks up within ~5 s, validates again, deletes
//     and acts on.
//
// Files instead of an HTTP API keep the backup container network-less
// towards Portal and make the trust boundary obvious: the only thing
// Portal can ever ask for is one of two fixed JSON documents.
package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxStatusBytes caps how much of status.json is read. The real file is
// ~2 KB; anything far bigger is broken or hostile.
const MaxStatusBytes = 64 << 10

// Actions a request may carry.
const (
	ActionBackup         = "backup"
	ActionRestoreStaging = "restore-staging"
)

// ---- status.json ----

// Run is the last_run object.
type Run struct {
	Started, Finished time.Time
	Status            string // "ok", "failed" or "" (unknown)
	Duration          time.Duration
	BytesAdded        int64 // -1 = not reported
	Error             string
}

// Snapshot is one entry of the snapshots list.
type Snapshot struct {
	ID   string // validated hex
	Time time.Time
}

// Check is last_check.
type Check struct {
	Time time.Time
	OK   *bool // nil = not reported
}

// RestoreTest is last_restore_test.
type RestoreTest struct {
	Time         time.Time
	OK           *bool
	FilesChecked int64 // -1 = not reported
	Detail       string
}

// StagingRestore is last_staging_restore.
type StagingRestore struct {
	Time     time.Time
	OK       *bool
	Snapshot string
	Path     string
	Detail   string
}

// Status is the parsed status.json. Every field is optional: the zero
// value (or -1 for counts) means "the file did not say".
type Status struct {
	Target         string // "local", "drive", or whatever the file says (shown escaped)
	Running        string // "", "backup", "restore-staging", "check", "restore-test"
	LastRun        *Run
	LastSuccess    time.Time
	SnapshotCount  int64 // -1 = not reported
	RepoSizeBytes  int64 // -1 = not reported
	Snapshots      []Snapshot
	LastCheck      *Check
	RestoreTest    *RestoreTest
	StagingRestore *StagingRestore
}

// ErrNoStatus means status.json does not exist yet (no backup has run, or
// the backup container is not deployed).
var ErrNoStatus = errors.New("backup: no status.json yet")

// ReadStatus reads and parses path. A missing file returns ErrNoStatus;
// an oversized or malformed one returns another error.
func ReadStatus(path string) (Status, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Status{}, ErrNoStatus
	}
	if err != nil {
		return Status{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxStatusBytes+1))
	if err != nil {
		return Status{}, err
	}
	if len(raw) > MaxStatusBytes {
		return Status{}, fmt.Errorf("backup: status.json is larger than %d KB", MaxStatusBytes>>10)
	}
	return ParseStatus(raw)
}

// ParseStatus decodes status.json leniently: a field with an unexpected
// type is ignored rather than failing the whole panel, unknown fields are
// ignored, and strings are length-capped. Only invalid JSON (or a
// top-level value that is not an object) is an error.
func ParseStatus(raw []byte) (Status, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return Status{}, errors.New("backup: status.json is not a JSON object")
	}
	st := Status{
		Target:        str(top["target"], 40),
		Running:       str(top["running"], 40),
		LastSuccess:   unixTime(top["last_success"]),
		SnapshotCount: count(top["snapshot_count"]),
		RepoSizeBytes: count(top["repo_size_bytes"]),
	}
	if o := obj(top["last_run"]); o != nil {
		r := &Run{
			Started: unixTime(o["started"]), Finished: unixTime(o["finished"]),
			Status: str(o["status"], 20), BytesAdded: count(o["bytes_added"]), Error: str(o["error"], 500),
		}
		if d, ok := num(o["duration_s"]); ok && d >= 0 && d < 1e7 {
			r.Duration = time.Duration(d * float64(time.Second))
		}
		st.LastRun = r
	}
	var snaps []json.RawMessage
	if json.Unmarshal(top["snapshots"], &snaps) == nil {
		for _, s := range snaps {
			o := obj(s)
			id := str(o["id"], 64)
			if o == nil || !snapshotRe.MatchString(id) {
				continue // never offer an id we would refuse in a request
			}
			st.Snapshots = append(st.Snapshots, Snapshot{ID: id, Time: unixTime(o["time"])})
			if len(st.Snapshots) == 20 { // the contract says 10; be tolerant but bounded
				break
			}
		}
	}
	if o := obj(top["last_check"]); o != nil {
		st.LastCheck = &Check{Time: unixTime(o["time"]), OK: boolean(o["ok"])}
	}
	if o := obj(top["last_restore_test"]); o != nil {
		st.RestoreTest = &RestoreTest{Time: unixTime(o["time"]), OK: boolean(o["ok"]),
			FilesChecked: count(o["files_checked"]), Detail: str(o["detail"], 300)}
	}
	if o := obj(top["last_staging_restore"]); o != nil {
		st.StagingRestore = &StagingRestore{Time: unixTime(o["time"]), OK: boolean(o["ok"]),
			Snapshot: str(o["snapshot"], 64), Path: str(o["path"], 200), Detail: str(o["detail"], 300)}
	}
	return st, nil
}

func obj(raw json.RawMessage) map[string]json.RawMessage {
	var o map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &o) != nil {
		return nil
	}
	return o
}

// str returns a string field, cut to max runes, or "" if it is not a string.
func str(raw json.RawMessage, max int) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	s = strings.ToValidUTF8(s, "�")
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}

func num(raw json.RawMessage) (float64, bool) {
	var f float64
	if len(raw) == 0 || json.Unmarshal(raw, &f) != nil {
		return 0, false
	}
	return f, true
}

// count is a non-negative integer field, or -1.
func count(raw json.RawMessage) int64 {
	f, ok := num(raw)
	if !ok || f < 0 || f > 1<<53 {
		return -1
	}
	return int64(f)
}

func boolean(raw json.RawMessage) *bool {
	var b bool
	if len(raw) == 0 || json.Unmarshal(raw, &b) != nil {
		return nil
	}
	return &b
}

// unixTime reads unix seconds. Zero, negative or absurd values (before
// 2020 or more than a day in the future of any sane clock) count as unset.
func unixTime(raw json.RawMessage) time.Time {
	f, ok := num(raw)
	if !ok || f < 1577836800 || f > 4102444800 { // 2020-01-01 .. 2100-01-01
		return time.Time{}
	}
	return time.Unix(int64(f), 0)
}

// ---- requests ----

// snapshotRe is the contract's snapshot id: 8 to 64 lowercase hex digits
// (restic short or full ids).
var snapshotRe = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// ValidSnapshot reports whether s may be sent in a restore request.
func ValidSnapshot(s string) bool { return s == "latest" || snapshotRe.MatchString(s) }

// Request is what Portal may ask the backup container to do.
type Request struct {
	Action   string `json:"action"`
	Snapshot string `json:"snapshot,omitempty"`
}

// Validate enforces the contract: exactly one of two shapes.
func (r Request) Validate() error {
	switch r.Action {
	case ActionBackup:
		if r.Snapshot != "" {
			return errors.New("a backup request takes no snapshot")
		}
	case ActionRestoreStaging:
		if !ValidSnapshot(r.Snapshot) {
			return errors.New(`snapshot must be "latest" or 8-64 lowercase hex characters`)
		}
	default:
		return errors.New("unknown action")
	}
	return nil
}

// Encode returns the exact bytes of the request file:
//
//	{"action":"backup"}
//	{"action":"restore-staging","snapshot":"latest"}
func (r Request) Encode() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r) // field order is the struct order; snapshot omitted when empty
}

// WriteRequest drops a request into dir and returns the file's path.
//
// Atomicity: the watcher in the other container polls dir for "*.json".
// If we created <nanos>.json directly and wrote into it, the watcher could
// see the file while it is still empty or half-written. Instead the bytes
// go into a temporary file whose name does not end in .json (the watcher
// ignores it), and rename() then gives it its final name. A rename within
// one directory is atomic: the watcher sees either no file or the complete
// file, never anything in between.
//
// The file name comes from the clock, never from user input, so no path
// can be smuggled in.
func WriteRequest(dir string, r Request, now time.Time) (string, error) {
	body, err := r.Encode()
	if err != nil {
		return "", err
	}
	if err := ensureDir(dir); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".portal-request-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(body); err != nil {
		return "", err
	}
	// CreateTemp makes 0600 files, and Portal runs with umask 077: set the
	// mode explicitly so the backup container (root, but with cap_drop ALL
	// it has no CAP_DAC_OVERRIDE) can read it.
	if err := tmp.Chmod(0o644); err != nil {
		return "", err
	}
	// fsync before rename: after a power cut the name must not point at
	// an empty file.
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	final := filepath.Join(dir, strconv.FormatInt(now.UnixNano(), 10)+".json")
	if err := os.Rename(tmpName, final); err != nil {
		return "", err
	}
	ok = true
	return final, nil
}

// ensureDir creates dir (0755) if it is missing. Mkdir's mode is filtered
// through the umask (077 for Portal), hence the explicit Chmod.
func ensureDir(dir string) error {
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.Chmod(dir, 0o755)
}

// Pending reports whether a request file is waiting in dir, and when the
// oldest one was written (from its name). A missing dir means none.
func Pending(dir string) (bool, time.Time) {
	f, err := os.Open(dir)
	if err != nil {
		return false, time.Time{}
	}
	defer f.Close()
	// Bounded read: there should be at most one or two entries.
	entries, _ := f.ReadDir(256)
	var oldest time.Time
	found := false
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".json") {
			continue
		}
		found = true
		if n, err := strconv.ParseInt(strings.TrimSuffix(name, ".json"), 10, 64); err == nil {
			if t := time.Unix(0, n); oldest.IsZero() || t.Before(oldest) {
				oldest = t
			}
		}
	}
	return found, oldest
}
