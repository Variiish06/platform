package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"portal/internal/procmem"
)

// fakeBreakdown is a scan with every group filled in.
func fakeBreakdown(now time.Time) procmem.Breakdown {
	const M = 1 << 20
	return procmem.Breakdown{
		At: now, Procs: 146, Took: 4 * time.Millisecond,
		Mem: procmem.Meminfo{Total: 7700 * M, Available: 5100 * M, SwapTotal: 4096 * M, SwapFree: 2828 * M},
		Groups: []procmem.Group{
			{Key: procmem.GContainers, Bytes: 655 * M, Count: 39, Projects: []procmem.Project{
				{Name: "dalal", Bytes: 300 * M, Items: []procmem.Item{{Label: "dalal-postgres-1", Bytes: 200 * M, Count: 7}, {Label: "dalal-api-1", Bytes: 100 * M, Count: 1}}},
				{Name: "platform", Bytes: 355 * M, Items: []procmem.Item{{Label: "platform-victoriametrics-1", Bytes: 355 * M, Count: 1}}}}},
			{Key: procmem.GClaude, Bytes: 1600 * M, Count: 6},
			{Key: procmem.GEngine, Bytes: 543 * M, Count: 28, Items: []procmem.Item{{Label: "dockerd", Bytes: 490 * M, Count: 1}}},
			{Key: procmem.GUser, Bytes: 105 * M, Count: 25, Items: []procmem.Item{{Label: "uvicorn", Sub: "uvicorn app <b>x</b>", PID: 42, Bytes: 41 * M}}},
			{Key: procmem.GSystem, Bytes: 217 * M, Count: 50},
			{Key: procmem.GKernel, Bytes: 371 * M},
			{Key: procmem.GCache, Bytes: 3000 * M},
			{Key: procmem.GFree, Bytes: 1150 * M},
			{Key: procmem.GUnaccounted, Bytes: 59 * M},
		},
		Sessions: []procmem.Session{{PID: 7, Cwd: "~/platform", Status: "idle", Name: "plat", Args: "claude --model x", Bytes: 430 * M, Procs: 4, Start: now.Add(-3 * time.Hour)}},
	}
}

func TestMemoryView(t *testing.T) {
	now := time.Now()
	v := buildMemory(fakeBreakdown(now), "", true, now)
	want := "5.0 GiB available of 7.5 GiB. Biggest: Claude Code sessions 1.6 GiB (6), Docker engine 543.0 MiB, other user processes"
	if v.State != "ok" || !strings.HasPrefix(v.Verdict, "5.0 GiB available of 7.5 GiB. Biggest: Claude Code sessions 1.6 GiB (6), Docker engine 543.0 MiB, ") {
		t.Fatalf("verdict %q (want like %q)", v.Verdict, want)
	}
	if !strings.Contains(v.Verdict, "kernel 371.0 MiB") {
		t.Errorf("third biggest should be the kernel: %q", v.Verdict)
	}
	if len(v.Segs) != 9 || v.Swap != "1.2 GiB of 4.0 GiB swap in use" {
		t.Errorf("segs %d swap %q", len(v.Segs), v.Swap)
	}
	if off := buildMemory(procmem.Breakdown{}, "", false, now); off.State != "off" {
		t.Error("unconfigured must be off")
	}
	if e := buildMemory(procmem.Breakdown{}, "boom", true, now); e.State != "error" || e.Error != "boom" {
		t.Error("first scan failing must show the error")
	}
	// Negative unaccounted is shown, not drawn.
	b := fakeBreakdown(now)
	b.Groups[8].Bytes = -40 << 20
	v = buildMemory(b, "", true, now)
	if len(v.Segs) != 8 || v.Groups[8].Bytes != "−40.0 MiB" || !v.Groups[8].Negative {
		t.Errorf("negative unaccounted: %d segs, %q", len(v.Segs), v.Groups[8].Bytes)
	}
	if fmtMem(0) != "0" || fmtMem(1536<<20) != "1.5 GiB" || fmtPct(1, 3) != "33%" || fmtPct(1, 0) != "–" {
		t.Error("formatters")
	}
}

func TestMemoryFragmentAndMetrics(t *testing.T) {
	s, token := newPhaseBServer(t, "")
	s.memScan = &procmem.Scanner{Proc: "/nonexistent"}
	if html := s.fragment("memory", s.memoryView()); !strings.Contains(html, "Reading the host") {
		t.Fatalf("loading state:\n%s", html)
	}
	s.mem = fakeBreakdown(time.Now())
	html := s.fragment("memory", s.memoryView())
	for _, w := range []string{"Claude Code sessions", "cs stop --idle", `data-k="mem-claude"`, "dalal-postgres-1", "~/platform",
		"&lt;b&gt;x&lt;/b&gt;", `class="mk-cache"`, "takes back the moment"} {
		if !strings.Contains(html, w) {
			t.Errorf("memory fragment lacks %q", w)
		}
	}
	if strings.Contains(html, "<b>x</b>") || strings.Contains(html, "style=") {
		t.Error("unescaped or inline style")
	}
	body := do(s.routes(), "GET", "/metrics", "", "", "").Body.String()
	for _, g := range procmem.GroupOrder {
		if !strings.Contains(body, fmt.Sprintf("portal_memory_group_bytes{group=%q}", g)) {
			t.Errorf("metrics lack group %s", g)
		}
	}
	if !strings.Contains(body, "portal_claude_sessions 6") {
		t.Error("claude sessions metric")
	}
	// Whole page has every section.
	page := do(s.routes(), "GET", "/", token, "", "").Body.String()
	for _, w := range []string{`id="status"`, `id="apps"`, `id="stats"`, `id="memory"`, `id="containers"`, `id="backup"`, `id="notify"`,
		`aria-live="polite"`, "/static/theme.js", `id="theme-toggle"`} {
		if !strings.Contains(page, w) {
			t.Errorf("dashboard lacks %s", w)
		}
	}
	if strings.Contains(page, "style=") || strings.Contains(page, "<script>") {
		t.Error("inline style/script would break the CSP")
	}
}

func TestStatusSentence(t *testing.T) {
	ok := statusInput{
		Apps:     appsData{Tiles: []appView{{Name: "Focal", State: "up"}, {Name: "Ritual", State: "up"}}, Up: 2, Total: 2},
		Backup:   backupData{Summary: "last backup 2 h ago"},
		Alerts:   alertsData{At: time.Now()},
		AlertsOn: true,
	}
	if v := buildStatus(ok); v.Level != "" || v.Plain != "All 2 apps up · last backup 2 h ago · no alerts firing" {
		t.Fatalf("healthy: %q %q", v.Level, v.Plain)
	}
	bad := ok
	bad.Apps = appsData{Tiles: []appView{{Name: "Focal", State: "up"}, {Name: "Bots", State: "down"}}, Up: 1, Total: 2}
	bad.Backup = backupData{Summary: "backup failed 01:20", SummaryLevel: "bad"}
	bad.Alerts = alertsData{At: time.Now(), List: []firingAlert{{Name: "ContainerDown", Severity: "urgent", Subject: "a"}, {Name: "ContainerDown", Severity: "urgent", Subject: "b"}}}
	bad.OnBattery, bad.BatteryPct = true, 80
	v := buildStatus(bad)
	if v.Level != "bad" || v.Plain != "Bots is down · backup failed 01:20 · 2 alerts firing: ContainerDown ×2 · on battery, 80%" {
		t.Fatalf("problems: %q %q", v.Level, v.Plain)
	}
	if v.Parts[3].Level != "warn" {
		t.Error("battery should be amber")
	}
	unk := ok
	unk.Alerts = alertsData{Failed: true}
	if v := buildStatus(unk); v.Level != "warn" || !strings.Contains(v.Plain, "Alerts unknown") || !strings.Contains(v.Plain, "· all 2 apps up") {
		t.Errorf("vm down: %q", v.Plain)
	}
	if joinNames([]string{"A", "B", "C"}) != "A, B and C" {
		t.Error("joinNames")
	}
}

func TestBackupSummary(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.Local)
	d := buildBackup(mustParse(t, fmt.Sprintf(`{"target":"local","last_run":{"started":%d,"finished":%d,"status":"failed","error":"x"},"last_success":%d}`,
		now.Add(-8*time.Hour).Unix(), now.Add(-7*time.Hour-40*time.Minute).Unix(), now.Add(-31*time.Hour).Unix())), nil, false, time.Time{}, now)
	if d.Summary != "backup failed 01:20" || d.SummaryLevel != "bad" {
		t.Fatalf("%q %q", d.Summary, d.SummaryLevel)
	}
	d = buildBackup(mustParse(t, fmt.Sprintf(`{"target":"local","last_run":{"finished":%d,"status":"ok"},"last_success":%d}`,
		now.Add(-2*time.Hour).Unix(), now.Add(-2*time.Hour).Unix())), nil, false, time.Time{}, now)
	if d.Summary != "last backup 2 h ago" || d.SummaryLevel != "" {
		t.Fatalf("%q %q", d.Summary, d.SummaryLevel)
	}
}

func TestContainerUptime(t *testing.T) {
	cases := map[string][2]string{
		"Up 2 days (healthy)":             {"2 days", ""},
		"Up 3 hours (unhealthy)":          {"3 hours", "unhealthy"},
		"Up 5 seconds (health: starting)": {"5 s", "starting"},
		"Exited (0) 6 minutes ago":        {"exited 6 min ago", ""},
		"Up About an hour":                {"1 hour", ""},
		"Created":                         {"created", ""},
	}
	for in, want := range cases {
		u, h := containerUptime(in)
		if u != want[0] || h != want[1] {
			t.Errorf("%q -> %q %q, want %q", in, u, h, want)
		}
	}
}

func TestStyledNotFound(t *testing.T) {
	s, _ := newPhaseBServer(t, "")
	h := s.routes()
	req := func(method, path, accept string) (int, string, string) {
		r := do(h, method, path, "", "", "")
		_ = accept
		return r.Code, r.Body.String(), r.Header().Get("Content-Security-Policy")
	}
	// do() sends no Accept header: plain 404 for scripts.
	if code, body, _ := req("GET", "/nope", ""); code != 404 || strings.Contains(body, "<html") {
		t.Fatalf("script 404: %d", code)
	}
	r := doAccept(h, "GET", "/nope", "text/html")
	if r.Code != 404 || !strings.Contains(r.Body.String(), "Nothing here") || r.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("browser 404: %d %s", r.Code, r.Body.String())
	}
	if r := doAccept(h, "GET", "/api/backup/run", "text/html"); r.Code != 405 {
		t.Fatalf("GET on a POST route must stay 405, got %d", r.Code)
	}
	if r := doAccept(h, "GET", "/login", "text/html"); r.Code != 200 || !strings.Contains(r.Body.String(), "Sign in") {
		t.Fatalf("login: %d", r.Code)
	}
}

// TestPreviewStates writes full dashboard pages for the unhappy states
// (an app down, a failed backup, ntfy off, alerts firing) to
// $PORTAL_PREVIEW_DIR, for screenshots. Skipped otherwise.
func TestPreviewStates(t *testing.T) {
	dir := os.Getenv("PORTAL_PREVIEW_DIR")
	if dir == "" {
		t.Skip("set PORTAL_PREVIEW_DIR to write preview pages")
	}
	s, token := newPhaseBServer(t, "")
	s.apps = mustApps(t, s) // one app on a closed port: down after a check
	s.apps.CheckAll(t.Context())
	now := time.Now()
	writeStatus(t, s, fmt.Sprintf(`{"target":"local","last_run":{"started":%d,"finished":%d,"status":"failed","duration_s":75,"bytes_added":0,"error":"rclone: 403 Forbidden"},
		"last_success":%d,"snapshot_count":3,"repo_size_bytes":9700000,"last_check":{"time":%d,"ok":true}}`,
		now.Add(-3*time.Hour).Unix(), now.Add(-3*time.Hour+time.Minute).Unix(), now.Add(-30*time.Hour).Unix(), now.Add(-10*time.Hour).Unix()))
	s.alerts = alertsData{At: now, List: []firingAlert{{Name: "ContainerDown", Severity: "urgent", Subject: "conveyor-api-1"}}}
	s.memScan = &procmem.Scanner{}
	s.mem = fakeBreakdown(now)
	page := do(s.routes(), "GET", "/", token, "", "").Body.String()
	if err := os.WriteFile(filepath.Join(dir, "unhappy.html"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
}

func doAccept(h http.Handler, method, target, accept string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Accept", accept)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
