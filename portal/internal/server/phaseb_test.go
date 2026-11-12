package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"portal/internal/apps"
	"portal/internal/backup"
	"portal/internal/docker"
	"portal/internal/metricsdb"
)

// Short aliases used by the helpers at the bottom.
type (
	backupStatus = backup.Status
	appsRegistry = apps.Registry
)

var (
	parseBackupStatus = backup.ParseStatus
	newAppsRegistry   = apps.NewRegistry
)

// ---- helpers ----

// fakeDocker is an engine with scripted containers and CPU counters.
type fakeDocker struct {
	mu         sync.Mutex
	list       []docker.Container
	cpu        map[string]docker.CPUSample
	mem        map[string]uint64
	listCalls  atomic.Int32
	statsCalls atomic.Int32
	listErr    error
}

func (f *fakeDocker) List(ctx context.Context) ([]docker.Container, error) {
	f.listCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]docker.Container(nil), f.list...), nil
}

func (f *fakeDocker) Stats(ctx context.Context, id string) (docker.Stats, error) {
	f.statsCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cpu[id]
	if !ok {
		return docker.Stats{}, errors.New("no such container")
	}
	return docker.Stats{CPU: c, Memory: f.mem[id], MemOK: true}, nil
}

func (f *fakeDocker) Restart(ctx context.Context, id string) error { return nil }

func id(n int) string { return fmt.Sprintf("%064x", n) }

func newPhaseBServer(t *testing.T, vmURL string) (*Server, string) {
	t.Helper()
	s := newTestServer(t, "")
	s.vm = metricsdb.New(vmURL)
	token, err := s.sessions.CreateWithTTL("test", "go test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return s, token
}

func do(h http.Handler, method, target, token, origin, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---- container metrics ----

func TestEscapeLabelAndFloat(t *testing.T) {
	if got := escapeLabel("a\"b\\c\nd\té"); got != `a\"b\\c\nd`+"\t"+`é` {
		t.Fatalf("escapeLabel = %q", got)
	}
	if escapeLabel("bad\xffutf8") != "bad�utf8" {
		t.Fatal("invalid UTF-8 not replaced")
	}
	for v, want := range map[float64]string{1.5: "1.5", 0: "0", 1e21: "1e+21", math.Inf(1): "+Inf"} {
		if got := formatFloat(v); got != want {
			t.Errorf("formatFloat(%v) = %s", v, got)
		}
	}
	if formatFloat(math.NaN()) != "NaN" {
		t.Error("NaN")
	}
}

func TestWriteContainerMetrics(t *testing.T) {
	var b strings.Builder
	writeContainerMetrics(&b, []docker.Container{
		{Name: "web", State: "running", CPUPercent: 12.5, CPUOK: true, MemBytes: 1048576, MemOK: true},
		{Name: "new", State: "running"},                     // first round: no CPU yet, stats failed
		{Name: `we"ird\name`, State: "exited", MemBytes: 5}, // stopped: up only
	})
	out := b.String()
	for _, want := range []string{
		"# TYPE portal_container_up gauge\n",
		`portal_container_up{name="web"} 1` + "\n",
		`portal_container_up{name="new"} 1` + "\n",
		`portal_container_up{name="we\"ird\\name"} 0` + "\n",
		`portal_container_cpu_percent{name="web"} 12.5` + "\n",
		`portal_container_memory_bytes{name="web"} 1048576` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	for _, not := range []string{`cpu_percent{name="new"}`, `memory_bytes{name="new"}`, `cpu_percent{name="we\"`, `memory_bytes{name="we\"`} {
		if strings.Contains(out, not) {
			t.Errorf("unexpected %q", not)
		}
	}
}

func TestCollectorCPUFromOwnSamples(t *testing.T) {
	s, _ := newPhaseBServer(t, "")
	f := &fakeDocker{
		list: []docker.Container{
			{ID: id(1), Name: "a", State: "running"},
			{ID: id(2), Name: "b", State: "running"},
			{ID: id(3), Name: "stopped", State: "exited"},
		},
		cpu: map[string]docker.CPUSample{
			id(1): {Total: 1e9, System: 100e9, CPUs: 4},
			id(2): {Total: 5e9, System: 100e9, CPUs: 4},
		},
		mem: map[string]uint64{id(1): 10 << 20, id(2): 20 << 20},
	}
	s.docker = f
	ctx := context.Background()

	s.collectContainers(ctx)
	list, fresh := s.containerSnapshot()
	if !fresh || len(list) != 3 || list[0].CPUOK || !list[0].MemOK || list[0].MemBytes != 10<<20 {
		t.Fatalf("first round: %+v", list)
	}
	if f.statsCalls.Load() != 2 {
		t.Fatalf("stats called %d times, want 2 (running only)", f.statsCalls.Load())
	}

	// Second round: a used 1 s of CPU while the host's 4 cores passed 40 s
	// (10 s wall): 1/40 * 4 * 100 = 10%. b restarted (counter went down).
	f.mu.Lock()
	f.cpu[id(1)] = docker.CPUSample{Total: 2e9, System: 140e9, CPUs: 4}
	f.cpu[id(2)] = docker.CPUSample{Total: 1e6, System: 140e9, CPUs: 4}
	f.mu.Unlock()
	s.collectContainers(ctx)
	list, _ = s.containerSnapshot()
	if !list[0].CPUOK || math.Abs(list[0].CPUPercent-10) > 1e-9 {
		t.Fatalf("a: %+v", list[0])
	}
	if list[1].CPUOK {
		t.Fatalf("b after restart should have no CPU value: %+v", list[1])
	}

	// Container a disappears: its series and its stored sample go.
	f.mu.Lock()
	f.list = f.list[1:]
	f.mu.Unlock()
	s.collectContainers(ctx)
	if _, ok := s.cpuPrev[id(1)]; ok {
		t.Fatal("sample of removed container kept")
	}

	// /metrics serves the cache: a scrape makes no Docker calls.
	before := f.listCalls.Load() + f.statsCalls.Load()
	rec := do(s.routes(), "GET", "/metrics", "", "", "")
	if rec.Code != 200 || f.listCalls.Load()+f.statsCalls.Load() != before {
		t.Fatalf("scrape: %d, docker calls %d -> %d", rec.Code, before, f.listCalls.Load()+f.statsCalls.Load())
	}
	body := rec.Body.String()
	if strings.Contains(body, `name="a"`) || !strings.Contains(body, `portal_container_up{name="stopped"} 0`) ||
		!strings.Contains(body, "portal_docker_up 1") || !strings.Contains(body, `portal_app_up`) {
		t.Fatalf("metrics:\n%s", body)
	}

	// Docker unreachable: keep the dashboard list, flag the error.
	f.mu.Lock()
	f.listErr = errors.New("dial unix: no such file")
	f.mu.Unlock()
	s.collectContainers(ctx)
	if !strings.Contains(do(s.routes(), "GET", "/metrics", "", "", "").Body.String(), "portal_docker_up 0") {
		t.Fatal("docker_up not 0 after failure")
	}
}

func TestMetricsAccessAndBackupCounter(t *testing.T) {
	s, token := newPhaseBServer(t, "")
	h := s.routes()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.9")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("proxied scrape without session: %d", rec.Code)
	}
	s.metrics.BackupRequest("backup", "accepted")
	out := do(h, "GET", "/metrics", token, "", "").Body.String()
	if !strings.Contains(out, `portal_backup_requests_total{action="backup",result="accepted"} 1`) ||
		!strings.Contains(out, `portal_backup_requests_total{action="restore-staging",result="busy"} 0`) {
		t.Fatalf("backup counter missing:\n%s", out)
	}
}

// ---- uptime ----

// fakeVMUptime answers the four uptime queries for app "focal": day 0..27
// missing, day 28 (yesterday) 50% over 4 samples, today 100% over 2.
func fakeVMUptime(t *testing.T, now time.Time) *httptest.Server {
	t.Helper()
	starts := dayStarts(now, uptimeDays)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		expr := q.Get("query")
		isCount := strings.Contains(expr, "count_over_time")
		switch r.URL.Path {
		case "/api/v1/query_range":
			if q.Get("step") != "86400s" || q.Get("start") != strconv.FormatInt(starts[1].Unix(), 10) {
				http.Error(w, `{"status":"error","errorType":"bad_data","error":"unexpected range"}`, 400)
				return
			}
			v := "0.5"
			if isCount {
				v = "4"
			}
			// The value at today's midnight describes yesterday (day 28).
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"app":"focal"},"values":[[%d,"%s"]]}]}}`,
				starts[29].Unix(), v)
		case "/api/v1/query":
			v := "1"
			if isCount {
				v = "2"
			}
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"app":"focal"},"value":[%d,"%s"]}]}}`, now.Unix(), v)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestFetchUptimeBucketsDays(t *testing.T) {
	loc := time.FixedZone("IST", 5*3600+1800)
	now := time.Date(2026, 11, 12, 9, 30, 0, 0, loc)
	vm := fakeVMUptime(t, now)
	defer vm.Close()
	d, err := fetchUptime(context.Background(), metricsdb.New(vm.URL), now)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Days[29].Equal(time.Date(2026, 11, 12, 0, 0, 0, 0, loc)) || !d.Days[0].Equal(time.Date(2026, 10, 14, 0, 0, 0, 0, loc)) {
		t.Fatalf("day starts %v .. %v", d.Days[0], d.Days[29])
	}
	a := d.Apps["focal"]
	if a == nil || a.Days[28].Ratio != 0.5 || a.Days[28].Samples != 4 || a.Days[29].Ratio != 1 || a.Days[29].Samples != 2 || a.Days[27].Samples != 0 {
		t.Fatalf("buckets: %+v", a)
	}
	// Weighted: (0.5*4 + 1*2) / 6 = 66.6% over 2 days.
	v := buildUptimeView(a, d.Days, "")
	if v.Label != "66.6% · 2d" || v.Level != "bad" || v.Segs[28].Level != "bad" || v.Segs[29].Level != "good" || v.Segs[0].Level != "none" {
		t.Fatalf("view: %+v", v)
	}
	if !strings.Contains(v.Segs[28].Title, "Wed 11 Nov: 50.00%") || !strings.Contains(v.Segs[29].Title, "(today)") {
		t.Fatalf("titles: %q %q", v.Segs[28].Title, v.Segs[29].Title)
	}
}

func TestUptimeViewEdgeCases(t *testing.T) {
	days := [uptimeDays]time.Time(dayStarts(time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC), uptimeDays))
	// No data at all.
	if v := buildUptimeView(nil, days, ""); v.Label != "No history yet" || len(v.Segs) != 30 || v.Segs[5].Level != "none" {
		t.Fatalf("empty: %+v", v)
	}
	if v := buildUptimeView(nil, days, "History unavailable"); v.Label != "History unavailable" || !strings.Contains(v.Aria, "history unavailable") {
		t.Fatalf("note: %+v", v)
	}
	// Only today: honest "since today".
	a := &appUptime{}
	a.Days[29] = uptimeDay{Ratio: 1, Samples: 20}
	if v := buildUptimeView(a, days, ""); v.Label != "100% · since today" || !strings.Contains(v.Aria, "less than a day") {
		t.Fatalf("today only: %+v", v)
	}
	// Full 30 days, one bad day: rounding goes down, never up to 100%.
	for i := range a.Days {
		a.Days[i] = uptimeDay{Ratio: 1, Samples: 2880}
	}
	a.Days[3] = uptimeDay{Ratio: 0.97, Samples: 2880}
	v := buildUptimeView(a, days, "")
	if v.Label != "99.9% · 30d" || v.Segs[3].Level != "warn" || v.Level != "good" {
		t.Fatalf("30d: %+v", v)
	}
	if !strings.Contains(v.Aria, "29 of 30 days") {
		t.Fatalf("aria: %s", v.Aria)
	}
	if fmtUptimePct(0.99999, 1) != "99.9%" || fmtUptimePct(1, 1) != "100%" || uptimeLevel(0.999) != "good" || uptimeLevel(0.9499) != "bad" {
		t.Fatal("pct/level")
	}
}

func TestUptimeVMDownKeepsPageWorking(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	s, token := newPhaseBServer(t, u)
	s.refreshUptime(context.Background())
	rec := do(s.routes(), "GET", "/", token, "", "")
	if rec.Code != 200 {
		t.Fatalf("dashboard: %d", rec.Code)
	}
	if s.vmErr == "" {
		t.Fatal("VM error not recorded")
	}
}

// ---- graphs ----

func TestGraphAllowlistAndRanges(t *testing.T) {
	for _, d := range graphDefs {
		if graphByName[d.Name] == nil || len(d.Queries) == 0 || FORMATS[d.Unit] == false {
			t.Errorf("graph %q incomplete (unit %q)", d.Name, d.Unit)
		}
	}
	for name, rg := range graphRanges {
		if n := int(rg.Dur / rg.Step); n < 300 || n > 400 {
			t.Errorf("range %s has %d points", name, n)
		}
	}
	if rateWindow(10*time.Second) != "300s" || rateWindow(2*time.Hour) != "7200s" {
		t.Error("rate window")
	}
}

// FORMATS mirrors the unit names graphs.js knows.
var FORMATS = map[string]bool{"percent": true, "bytes": true, "bytes_s": true, "celsius": true, "seconds": true, "number": true}

func TestGraphEndpoint(t *testing.T) {
	var calls atomic.Int32
	var lastQuery atomic.Value
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		lastQuery.Store(q.Get("query"))
		start, _ := strconv.ParseFloat(q.Get("start"), 64)
		step := 10.0
		// Three containers; "c" has the highest latest value.
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"name":"a"},"values":[[%.0f,"1"],[%.0f,"2"]]},
			{"metric":{"name":"b"},"values":[[%.0f,"NaN"]]},
			{"metric":{"name":"c"},"values":[[%.0f,"50"]]}]}}`, start, start+step, start, start+2*step)
	}))
	defer vm.Close()
	s, token := newPhaseBServer(t, vm.URL)
	h := s.routes()

	rec := do(h, "GET", "/api/graphs/containers_cpu?range=1h", token, "", "")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var res struct {
		Name   string
		Unit   string
		Step   int64
		Series []struct{ Label string }
		Data   [][]*float64
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Unit != "percent" || res.Step != 10 || len(res.Data) != 4 || len(res.Data[0]) != 361 {
		t.Fatalf("shape: unit %s step %d, %d arrays, %d points", res.Unit, res.Step, len(res.Data), len(res.Data[0]))
	}
	// Sorted by latest value: c (50), a (2), b (none).
	if res.Series[0].Label != "c" || res.Series[1].Label != "a" || res.Series[2].Label != "b" {
		t.Fatalf("order %+v", res.Series)
	}
	if res.Data[2][0] == nil || *res.Data[2][0] != 1 || res.Data[2][2] != nil || res.Data[3][0] != nil {
		t.Fatalf("alignment / NaN -> null broken")
	}
	if !strings.Contains(lastQuery.Load().(string), "portal_container_cpu_percent") {
		t.Fatalf("query %q", lastQuery.Load())
	}

	// Cached: the same name+range within the TTL does not hit VM again.
	n := calls.Load()
	do(h, "GET", "/api/graphs/containers_cpu?range=1h", token, "", "")
	if calls.Load() != n {
		t.Fatal("cache miss")
	}
	// $w is substituted with the rate window, never sent literally.
	do(h, "GET", "/api/graphs/cpu?range=30d", token, "", "")
	if q := lastQuery.Load().(string); strings.Contains(q, "$w") || !strings.Contains(q, "[7200s]") {
		t.Fatalf("cpu query %q", q)
	}

	for target, code := range map[string]int{
		"/api/graphs/nope?range=1h":              404,
		"/api/graphs/cpu?range=2h":               400,
		"/api/graphs/cpu?range=1h%3Bdrop":        400,
		"/api/graphs/cpu?query=up&range=1h":      200, // extra params are ignored, never forwarded
		"/api/graphs/" + url.PathEscape("x{y}"): 404,
	} {
		if rec := do(h, "GET", target, token, "", ""); rec.Code != code {
			t.Errorf("%s: %d, want %d", target, rec.Code, code)
		}
	}
}

func TestGraphEndpointVMDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	s, token := newPhaseBServer(t, u)
	h := s.routes()
	if rec := do(h, "GET", "/api/graphs/cpu?range=1h", token, "", ""); rec.Code != 503 || !strings.Contains(rec.Body.String(), "not reachable") {
		t.Fatalf("vm down: %d %s", rec.Code, rec.Body)
	}
	// The page itself still loads, quickly, with the banner visible.
	start := time.Now()
	rec := do(h, "GET", "/graphs", token, "", "")
	if rec.Code != 200 || time.Since(start) > time.Second {
		t.Fatalf("graphs page: %d in %v", rec.Code, time.Since(start))
	}
	page := rec.Body.String()
	if !strings.Contains(page, `data-vm="down"`) || strings.Contains(page, `data-vm="down" hidden`) {
		t.Fatal("banner not shown")
	}
	for _, want := range []string{"/static/vendor/uPlot.iife.min.js", "/static/vendor/uPlot.min.css", "/static/graphs.js", `data-graph="containers_memory"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	if strings.Contains(page, "style=") || strings.Contains(page, "<script>") {
		t.Error("inline style/script would break the CSP")
	}
}

func TestGraphCacheSingleFlight(t *testing.T) {
	c := newGraphCache()
	var n atomic.Int32
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.get("k", time.Now(), func() (int, []byte) {
				n.Add(1)
				time.Sleep(20 * time.Millisecond)
				return 200, []byte("x")
			})
		}()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Fatalf("fetched %d times", n.Load())
	}
	// Expired entries are fetched again.
	if code, _ := c.get("k", time.Now().Add(time.Minute), func() (int, []byte) { n.Add(1); return 200, nil }); code != 200 || n.Load() != 2 {
		t.Fatal("expiry")
	}
}

func TestStaticVendorServed(t *testing.T) {
	s, _ := newPhaseBServer(t, "")
	h := s.routes()
	for _, p := range []string{"/static/vendor/uPlot.iife.min.js", "/static/vendor/uPlot.min.css", "/static/graphs.js"} {
		rec := do(h, "GET", p, "", "", "")
		if rec.Code != 200 || rec.Body.Len() < 500 {
			t.Errorf("%s: %d (%d bytes)", p, rec.Code, rec.Body.Len())
		}
	}
}

// ---- backups ----

const origin = "http://exp.lab"

func writeStatus(t *testing.T, s *Server, body string) {
	t.Helper()
	if err := os.MkdirAll(s.backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.backupStatusPath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requestFiles(t *testing.T, s *Server) []string {
	t.Helper()
	entries, _ := os.ReadDir(s.backupRequestsDir())
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestBackupRequestFlow(t *testing.T) {
	s, token := newPhaseBServer(t, "")
	h := s.routes()

	rec := do(h, "POST", "/api/backup/run", token, origin, "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"ok":true}` {
		t.Fatalf("run: %d %s", rec.Code, rec.Body)
	}
	files := requestFiles(t, s)
	if len(files) != 1 || !strings.HasSuffix(files[0], ".json") {
		t.Fatalf("files %v", files)
	}
	b, _ := os.ReadFile(filepath.Join(s.backupRequestsDir(), files[0]))
	if string(b) != `{"action":"backup"}` {
		t.Fatalf("content %q", b)
	}

	// Pending request -> 409, no second file.
	if rec := do(h, "POST", "/api/backup/restore-staging", token, origin, `{"snapshot":"latest"}`); rec.Code != 409 {
		t.Fatalf("pending: %d", rec.Code)
	}
	// The panel says "requested".
	if html := s.fragment("backup", s.backupView()); !strings.Contains(html, "Requested") || !strings.Contains(html, "disabled") {
		t.Fatalf("pending panel:\n%s", html)
	}

	// The watcher consumed it, but the 30 s rate limit still applies.
	os.Remove(filepath.Join(s.backupRequestsDir(), files[0]))
	if rec := do(h, "POST", "/api/backup/run", token, origin, ""); rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d", rec.Code)
	}
	s.backupLastAccepted = time.Now().Add(-time.Minute)

	// status.json says something is running -> 409.
	writeStatus(t, s, `{"running":"check"}`)
	if rec := do(h, "POST", "/api/backup/run", token, origin, ""); rec.Code != 409 || !strings.Contains(rec.Body.String(), "Checking") {
		t.Fatalf("running: %d %s", rec.Code, rec.Body)
	}
	writeStatus(t, s, `{"running":""}`)

	// Restore with a specific snapshot: exact content.
	rec = do(h, "POST", "/api/backup/restore-staging", token, origin, `{"snapshot":"a1b2c3d4e5"}`)
	if rec.Code != 200 {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	files = requestFiles(t, s)
	b, _ = os.ReadFile(filepath.Join(s.backupRequestsDir(), files[0]))
	if len(files) != 1 || string(b) != `{"action":"restore-staging","snapshot":"a1b2c3d4e5"}` {
		t.Fatalf("restore file %v %q", files, b)
	}
	os.Remove(filepath.Join(s.backupRequestsDir(), files[0]))
	s.backupLastAccepted = time.Time{}

	// Empty body -> latest.
	if rec := do(h, "POST", "/api/backup/restore-staging", token, origin, ""); rec.Code != 200 {
		t.Fatalf("restore default: %d", rec.Code)
	}
	files = requestFiles(t, s)
	b, _ = os.ReadFile(filepath.Join(s.backupRequestsDir(), files[0]))
	if string(b) != `{"action":"restore-staging","snapshot":"latest"}` {
		t.Fatalf("default content %q", b)
	}
	os.Remove(filepath.Join(s.backupRequestsDir(), files[0]))
	s.backupLastAccepted = time.Time{}

	// Invalid snapshots: 400 and no file.
	for _, body := range []string{
		`{"snapshot":"../../etc/passwd"}`, `{"snapshot":"ABCDEF12"}`, `{"snapshot":"abc"}`,
		`{"snapshot":"latest","path":"/"}`, `{"snapshot":5}`, `not json`, `{"snapshot":"` + strings.Repeat("a", 65) + `"}`,
		`{"snapshot":"` + strings.Repeat("a", 2000) + `"}`,
	} {
		if rec := do(h, "POST", "/api/backup/restore-staging", token, origin, body); rec.Code != 400 {
			t.Errorf("%.40s: %d", body, rec.Code)
		}
	}
	if f := requestFiles(t, s); len(f) != 0 {
		t.Fatalf("invalid requests left files: %v", f)
	}

	s.metrics.mu.Lock()
	acc, inv, busy := s.metrics.backupReqs["backup|accepted"], s.metrics.backupReqs["restore-staging|invalid"], s.metrics.backupReqs["restore-staging|busy"]
	s.metrics.mu.Unlock()
	if acc != 1 || inv != 8 || busy != 1 {
		t.Errorf("metrics accepted=%d invalid=%d busy=%d", acc, inv, busy)
	}
}

func TestNewRoutesAuth(t *testing.T) {
	s, token := newPhaseBServer(t, "")
	h := s.routes()
	// No session.
	if rec := do(h, "GET", "/graphs", "", "", ""); rec.Code != 302 || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("/graphs: %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/graphs/cpu?range=1h", "", "", ""); rec.Code != 401 {
		t.Errorf("/api/graphs: %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/graphs/x", "", "", ""); rec.Code != 401 {
		t.Errorf("/api/graphs/x: %d", rec.Code)
	}
	for _, p := range []string{"/api/backup/run", "/api/backup/restore-staging"} {
		if rec := do(h, "POST", p, "", origin, ""); rec.Code != 401 {
			t.Errorf("%s no session: %d", p, rec.Code)
		}
		if rec := do(h, "POST", p, "bogus", origin, ""); rec.Code != 401 {
			t.Errorf("%s bad session: %d", p, rec.Code)
		}
		// Session but wrong or missing Origin (cross-site, or a sibling app).
		for _, o := range []string{"", "http://evil.example", "http://focal.exp.lab", "https://exp.lab"} {
			if rec := do(h, "POST", p, token, o, ""); rec.Code != 403 {
				t.Errorf("%s origin %q: %d", p, o, rec.Code)
			}
		}
		if rec := do(h, "GET", p, token, origin, ""); rec.Code != 405 {
			t.Errorf("GET %s: %d", p, rec.Code)
		}
	}
	if f := requestFiles(t, s); len(f) != 0 {
		t.Fatalf("refused requests left files: %v", f)
	}
}

// ---- backup panel ----

func TestBackupPanel(t *testing.T) {
	s, token := newPhaseBServer(t, "")
	// Missing file.
	if html := s.fragment("backup", s.backupView()); !strings.Contains(html, "No backup has run yet") {
		t.Fatalf("missing:\n%s", html)
	}
	// Malformed.
	writeStatus(t, s, "{oops")
	if html := s.fragment("backup", s.backupView()); !strings.Contains(html, "could not be read") {
		t.Fatalf("malformed:\n%s", html)
	}
	// Oversized.
	writeStatus(t, s, `{"target":"`+strings.Repeat("x", 70<<10)+`"}`)
	if html := s.fragment("backup", s.backupView()); !strings.Contains(html, "larger than") {
		t.Fatalf("oversized:\n%s", html)
	}
	// Failed run with hostile text; stale success.
	now := time.Now()
	writeStatus(t, s, fmt.Sprintf(`{"target":"local","running":"backup",
		"last_run":{"started":%d,"finished":%d,"status":"failed","duration_s":75,"bytes_added":2048,"error":"<script>alert(1)</script>"},
		"last_success":%d,"snapshot_count":3,"repo_size_bytes":1073741824,
		"snapshots":[{"id":"a1b2c3d4e5f6","time":%d}],
		"last_check":{"time":%d,"ok":true},
		"last_restore_test":{"time":%d,"ok":false,"files_checked":12,"detail":"x"},
		"last_staging_restore":{"time":%d,"ok":true,"snapshot":"a1b2c3d4","path":"/restore-staging/20261112T0300Z-a1b2c3d4"},
		"unknown":{"x":1}}`,
		now.Add(-time.Hour).Unix(), now.Add(-59*time.Minute).Unix(), now.Add(-30*time.Hour).Unix(),
		now.Add(-30*time.Hour).Unix(), now.Add(-48*time.Hour).Unix(), now.Add(-72*time.Hour).Unix(), now.Add(-2*time.Hour).Unix()))
	html := s.fragment("backup", s.backupView())
	for _, want := range []string{
		"Local disk", "Backing up…", "The last backup failed.", "Failed · 59 min ago", "took 1 min 15 s", "2 KB added",
		"&lt;script&gt;", "1.00 GB", "3 snapshots", "integrity passed", "failed · 12 files checked",
		"~/restore-staging/20261112T0300Z-a1b2c3d4", "older than 26 h", `class="backup warn"`,
		`<option value="a1b2c3d4e5f6">a1b2c3d4 · `, "disabled",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("panel lacks %q", want)
		}
	}
	if strings.Contains(html, "<script>") || strings.Contains(html, "style=") {
		t.Fatal("unescaped content or inline style")
	}
	// Whole dashboard renders with the new panel and the uptime bars.
	s.apps = mustApps(t, s)
	rec := do(s.routes(), "GET", "/", token, "", "")
	page := rec.Body.String()
	for _, want := range []string{`id="backup"`, `id="restore-dialog"`, `class="upbar"`, `href="/graphs"`, "never touches live data"} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard lacks %s", want)
		}
	}
	if strings.Contains(page, "style=") {
		t.Error("inline style on dashboard")
	}
}

func TestBuildBackupFresh(t *testing.T) {
	now := time.Now()
	writeless := buildBackup(mustParse(t, fmt.Sprintf(`{"target":"drive","last_run":{"finished":%d,"status":"ok"},"last_success":%d}`,
		now.Add(-time.Hour).Unix(), now.Add(-time.Hour).Unix())), nil, false, time.Time{}, now)
	if writeless.Warn || writeless.Target != "Google Drive" || writeless.Headline != "Backups are up to date." {
		t.Fatalf("%+v", writeless)
	}
	// The real file on a fresh install: nulls everywhere.
	fresh := buildBackup(mustParse(t, `{"target":"local","running":"","last_run":null,"last_success":null,"snapshot_count":0,
		"repo_size_bytes":0,"snapshots":[],"last_check":null,"last_restore_test":null,"last_staging_restore":null}`), nil, false, time.Time{}, now)
	if fresh.Warn || fresh.Headline != "No backup has run yet." || fresh.Target != "Local disk" {
		t.Fatalf("fresh install: %+v", fresh)
	}
	if stagingPath("/home/u/restore-staging/x-1") != "~/restore-staging/x-1" || stagingPath("/restore-staging/y") != "~/restore-staging/y" {
		t.Fatal("stagingPath")
	}
	if got := shortDetail("first line\nsecond <b>"); got != "first line" {
		t.Fatalf("shortDetail %q", got)
	}
	if fmtDuration(45*time.Second) != "45 s" || fmtDuration(3723*time.Second) != "1 h 02 min" || fmtSize(-1) != "–" {
		t.Fatal("formatters")
	}
}

func mustParse(t *testing.T, s string) (st backupStatus) {
	t.Helper()
	st, err := parseBackupStatus([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func mustApps(t *testing.T, s *Server) *appsRegistry {
	t.Helper()
	p := filepath.Join(s.cfg.DataDir, "apps.yaml")
	os.WriteFile(p, []byte("apps:\n  - id: focal\n    name: Focal\n    upstream: 127.0.0.1:1\n"), 0o600)
	r, err := newAppsRegistry(p, "exp.lab")
	if err != nil {
		t.Fatal(err)
	}
	return r
}
