package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"portal/internal/backup"
)

// ---- Backups panel ----
//
// Portal's whole role in backups is reading one file and writing another
// (see package backup). The panel is refreshed in the background every
// 10 s; dashboards get the new HTML over the event stream when it changes.

const (
	backupPollInterval = 10 * time.Second
	// backupStaleAfter: a daily 03:00 backup plus two hours of slack. Same
	// threshold as the BackupStale alert.
	backupStaleAfter = 26 * time.Hour
	// backupRequestGap: at least this long between two accepted requests,
	// so a double tap (or a stuck script) cannot queue a pile of runs.
	backupRequestGap = 30 * time.Second
)

func (s *Server) backupStatusPath() string  { return filepath.Join(s.backupDir, "status.json") }
func (s *Server) backupRequestsDir() string { return filepath.Join(s.backupDir, "requests") }

// kv is one labelled line in the panel.
type kv struct {
	Label, Value string
	Sub          string // smaller second line, optional
	SubFull      string // tooltip for Sub when it was shortened
	State        string // "", "ok", "warn", "bad"
	TS           int64  // when set, app.js keeps Value's "x ago" current
	Abs          string // tooltip with the absolute time
}

type snapOption struct {
	ID, Label string
}

type backupData struct {
	State    string // "none" (no status.json), "error" (unreadable), "ok"
	Error    string
	Target   string
	Running  string // "" or a human description of what runs now
	Pending  bool   // a request file is waiting for the backup container
	PendingAgo string
	Warn     bool   // stale or failed: the panel turns amber/red
	Headline string // one line under the title
	Rows     []kv
	Snaps    []snapOption
}

func targetName(t string) string {
	switch t {
	case "local":
		return "Local disk"
	case "drive":
		return "Google Drive"
	case "":
		return "Unknown target"
	}
	return t // shown escaped by html/template
}

func runningName(r string) string {
	switch r {
	case "backup":
		return "Backing up"
	case "restore-staging":
		return "Restoring to staging"
	case "check":
		return "Checking the repository"
	case "restore-test":
		return "Testing a restore"
	}
	return "Working: " + r
}

// fmtDuration: "45 s", "3 min 12 s", "1 h 05 min".
func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min %02d s", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d h %02d min", int(d.Hours()), int(d.Minutes())%60)
}

// fmtSize is like fmtBytes but says "0 B" instead of a dash.
func fmtSize(b int64) string {
	switch {
	case b < 0:
		return "–"
	case b < 1<<10:
		return fmt.Sprintf("%d B", b)
	case b < 1<<20:
		return fmt.Sprintf("%.0f KB", float64(b)/(1<<10))
	case b < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%.2f GB", float64(b)/(1<<30))
}

// absTime: "Thu 12 Nov 03:00 IST" (the zone comes from TZ).
func absTime(t time.Time) string { return t.Local().Format("Mon 2 Jan 15:04 MST") }

func okState(ok *bool) (string, string) {
	switch {
	case ok == nil:
		return "", "unknown"
	case *ok:
		return "ok", "passed"
	}
	return "bad", "failed"
}

// stagingPath shows the restore folder the way the owner knows it,
// whether the file names the container path (/restore-staging/x) or the
// host path (/home/<user>/restore-staging/x).
func stagingPath(p string) string {
	if i := strings.LastIndex(p, "/restore-staging/"); i >= 0 {
		return "~" + p[i:]
	}
	return p
}

// shortDetail keeps the first line of a (possibly multi-line, log-like)
// detail text, shortened for the panel.
func shortDetail(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return truncateRunes(strings.TrimSpace(line), 120)
}

// buildBackup turns the status file (or its absence) into the panel.
func buildBackup(st backup.Status, readErr error, pending bool, pendingAt, now time.Time) backupData {
	d := backupData{State: "ok", Pending: pending}
	if pending && !pendingAt.IsZero() {
		d.PendingAgo = ago(pendingAt, now)
	}
	switch {
	case errors.Is(readErr, backup.ErrNoStatus):
		d.State, d.Headline = "none", "No backup has run yet."
		return d
	case readErr != nil:
		d.State, d.Warn = "error", true
		d.Error = "status.json could not be read: " + readErr.Error()
		return d
	}
	d.Target = targetName(st.Target)
	if st.Running != "" {
		d.Running = runningName(st.Running)
	}

	at := func(label string, t time.Time, sub, state string) kv {
		if t.IsZero() {
			return kv{Label: label, Value: "never", Sub: sub, State: state}
		}
		return kv{Label: label, Value: ago(t, now), TS: t.Unix(), Abs: absTime(t), Sub: sub, State: state}
	}

	// Last success, and the staleness warning. A fresh install where
	// nothing has run yet is not "stale" (the first scheduled run is
	// still ahead); once anything has run, no success within 26 h is.
	neverRan := st.LastRun == nil && st.LastSuccess.IsZero()
	stale := !neverRan && (st.LastSuccess.IsZero() || now.Sub(st.LastSuccess) > backupStaleAfter)
	succ := at("Last success", st.LastSuccess, "", "ok")
	if !st.LastSuccess.IsZero() {
		succ.Sub = absTime(st.LastSuccess)
	}
	if stale {
		succ.State = "warn"
		if st.LastSuccess.IsZero() {
			succ.Sub = "no successful backup yet"
		} else {
			succ.Sub += " · older than 26 h"
		}
	}

	// Last run.
	failed := false
	if r := st.LastRun; r != nil {
		when := r.Finished
		if when.IsZero() {
			when = r.Started
		}
		state, word := "", "unknown"
		switch r.Status {
		case "ok":
			state, word = "ok", "OK"
		case "failed":
			state, word, failed = "bad", "Failed", true
		}
		sub := []string{}
		if r.Duration > 0 {
			sub = append(sub, "took "+fmtDuration(r.Duration))
		}
		if r.BytesAdded >= 0 {
			sub = append(sub, fmtSize(r.BytesAdded)+" added")
		}
		row := at("Last run", when, strings.Join(sub, " · "), state)
		row.Value = word + " · " + row.Value
		d.Rows = append(d.Rows, row)
		if failed && r.Error != "" {
			d.Rows = append(d.Rows, kv{Label: "Error", Value: r.Error, State: "bad"})
		}
	} else {
		d.Rows = append(d.Rows, kv{Label: "Last run", Value: "never"})
	}
	d.Rows = append(d.Rows, succ)

	repo := kv{Label: "Repository", Value: fmtSize(st.RepoSizeBytes)}
	if st.SnapshotCount >= 0 {
		repo.Sub = fmt.Sprintf("%d snapshots", st.SnapshotCount)
		if st.SnapshotCount == 1 {
			repo.Sub = "1 snapshot"
		}
	}
	d.Rows = append(d.Rows, repo)

	if c := st.LastCheck; c != nil {
		state, word := okState(c.OK)
		d.Rows = append(d.Rows, at("Last check", c.Time, "integrity "+word, state))
	} else {
		d.Rows = append(d.Rows, kv{Label: "Last check", Value: "never"})
	}
	if t := st.RestoreTest; t != nil {
		state, word := okState(t.OK)
		sub := word
		if t.FilesChecked >= 0 {
			sub += fmt.Sprintf(" · %d files checked", t.FilesChecked)
		}
		if t.Detail != "" {
			sub += " · " + shortDetail(t.Detail)
		}
		row := at("Restore test", t.Time, sub, state)
		row.SubFull = t.Detail
		d.Rows = append(d.Rows, row)
	} else {
		d.Rows = append(d.Rows, kv{Label: "Restore test", Value: "never"})
	}
	if sr := st.StagingRestore; sr != nil {
		state, word := okState(sr.OK)
		parts := []string{word}
		if sr.Snapshot != "" {
			parts = append(parts, "snapshot "+sr.Snapshot)
		}
		if sr.Path != "" {
			parts = append(parts, stagingPath(sr.Path))
		}
		if sr.Detail != "" {
			parts = append(parts, shortDetail(sr.Detail))
		}
		row := at("Staging restore", sr.Time, strings.Join(parts, " · "), state)
		row.SubFull = sr.Detail
		d.Rows = append(d.Rows, row)
	}

	for _, sn := range st.Snapshots {
		label := sn.ID
		if len(label) > 8 {
			label = label[:8]
		}
		if !sn.Time.IsZero() {
			label += " · " + absTime(sn.Time)
		}
		d.Snaps = append(d.Snaps, snapOption{ID: sn.ID, Label: label})
	}

	d.Warn = stale || failed
	switch {
	case failed:
		d.Headline = "The last backup failed."
	case stale:
		d.Headline = "No successful backup in the last 26 hours."
	case neverRan:
		d.Headline = "No backup has run yet."
	default:
		d.Headline = "Backups are up to date."
	}
	return d
}

// backupState is one read of the shared directory.
type backupState struct {
	status    backup.Status
	err       error // backup.ErrNoStatus, a read/parse error, or nil
	pending   bool
	pendingAt time.Time
}

// readBackup reads the current state from disk: one small file and one
// directory listing, cheap enough to do on every poll and every request.
func (s *Server) readBackup() backupState {
	var b backupState
	b.status, b.err = backup.ReadStatus(s.backupStatusPath())
	b.pending, b.pendingAt = backup.Pending(s.backupRequestsDir())
	return b
}

func (s *Server) backupView() backupData {
	b := s.readBackup()
	return buildBackup(b.status, b.err, b.pending, b.pendingAt, time.Now())
}

func (s *Server) publishBackup() {
	s.hub.Publish("backup", strings.TrimSpace(s.fragment("backup", s.backupView())))
}

// ---- POST /api/backup/run and /api/backup/restore-staging ----

func (s *Server) handleBackupRun(w http.ResponseWriter, r *http.Request) {
	s.handleBackupRequest(w, r, backup.ActionBackup)
}

func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	s.handleBackupRequest(w, r, backup.ActionRestoreStaging)
}

// handleBackupRequest validates a dashboard button press and, if all is
// well, drops ONE request file. It never runs anything itself.
//
// Order of checks: who (session), from where (Origin), what (body),
// whether now is a good time (busy, rate limit), then act.
func (s *Server) handleBackupRequest(w http.ResponseWriter, r *http.Request, action string) {
	reply := func(code int, result string, body map[string]any) {
		if result != "" {
			s.metrics.BackupRequest(action, result)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(body)
	}
	fail := func(code int, result, msg string) { reply(code, result, map[string]any{"ok": false, "error": msg}) }

	if _, ok := s.sessionToken(r); !ok {
		fail(http.StatusUnauthorized, "", "not signed in")
		return
	}
	if !s.sameOrigin(r) {
		fail(http.StatusForbidden, "", "cross-origin request refused")
		return
	}

	req := backup.Request{Action: action}
	if action == backup.ActionRestoreStaging {
		// Optional JSON body {"snapshot": "latest" | "<hex id>"}.
		var body struct {
			Snapshot string `json:"snapshot"`
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
		if err != nil {
			fail(http.StatusBadRequest, "invalid", "request body too large")
			return
		}
		if len(strings.TrimSpace(string(raw))) > 0 {
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&body); err != nil {
				fail(http.StatusBadRequest, "invalid", `body must be {"snapshot": "latest" or a snapshot id}`)
				return
			}
		}
		req.Snapshot = body.Snapshot
		if req.Snapshot == "" {
			req.Snapshot = "latest"
		}
	}
	if err := req.Validate(); err != nil {
		fail(http.StatusBadRequest, "invalid", err.Error())
		return
	}

	// One request at a time, end to end. The lock covers check-then-write,
	// so two simultaneous clicks cannot both pass the "nothing pending"
	// test.
	s.backupMu.Lock()
	defer s.backupMu.Unlock()

	cur := s.readBackup()
	if cur.err == nil && cur.status.Running != "" {
		fail(http.StatusConflict, "busy", runningName(cur.status.Running)+" right now. Try again when it has finished.")
		return
	}
	if cur.pending {
		fail(http.StatusConflict, "busy", "A request is already waiting for the backup service.")
		return
	}
	if wait := backupRequestGap - time.Since(s.backupLastAccepted); wait > 0 {
		w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		fail(http.StatusTooManyRequests, "rate_limited", "Please wait a few seconds between requests.")
		return
	}

	path, err := backup.WriteRequest(s.backupRequestsDir(), req, time.Now())
	if err != nil {
		log.Printf("backup request: %v", err)
		fail(http.StatusInternalServerError, "error", "Could not write the request file.")
		return
	}
	s.backupLastAccepted = time.Now()
	log.Printf("backup request %s (%s) by %s", filepath.Base(path), action, clientIP(r))
	reply(http.StatusOK, "accepted", map[string]any{"ok": true})
	go s.publishBackup() // show "requested" on every open dashboard now
}
