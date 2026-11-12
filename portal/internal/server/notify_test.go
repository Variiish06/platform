package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"portal/internal/notify"
	"portal/internal/store"
)

const (
	testPortalToken = "tk_portalportalportalportalporta"
	testCallerToken = "tk_callercallercallercallercalle"
)

// fakeNtfy pretends to be ntfy: callerToken may publish to "general" only.
type fakeNtfy struct {
	mu    sync.Mutex
	auths []string // Authorization header of every publish
}

func (f *fakeNtfy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet { // the poller
		io.WriteString(w, `{"id":"m1","time":1700000000,"event":"message","topic":"general","title":"<b>hi</b>","message":"<script>alert(1)</script>","priority":5}`+"\n")
		return
	}
	f.mu.Lock()
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.mu.Unlock()
	var body struct{ Topic string }
	json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.Header.Get("Authorization") != "Bearer "+testCallerToken:
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"code":40101,"http":401,"error":"unauthorized"}`)
	case body.Topic != "general":
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"code":40301,"http":403,"error":"forbidden"}`)
	default:
		io.WriteString(w, `{"id":"new1","time":1,"event":"message","topic":"general","message":"x"}`)
	}
}

func newTestServer(t *testing.T, ntfyURL string) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "apps.yaml"), []byte("apps: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "portal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := New(Config{Domain: "exp.lab", DataDir: dir, NtfyURL: ntfyURL, NtfyToken: testPortalToken}, db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func postNotify(h http.Handler, auth, body string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/notify", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestNotifyEndpoint(t *testing.T) {
	f := &fakeNtfy{}
	ntfy := httptest.NewServer(f)
	defer ntfy.Close()
	s := newTestServer(t, ntfy.URL)
	h := s.routes()
	good := `{"topic":"general","title":"Hello","message":"world","priority":"urgent","tags":["tada"]}`

	cases := []struct {
		name, auth, body string
		code             int
		errPart          string
	}{
		{"ok", "Bearer " + testCallerToken, good, 200, ""},
		{"no token", "", good, 401, "Bearer"},
		{"malformed token", "Bearer tk_short", good, 401, "Bearer"},
		{"basic auth", "Basic dXNlcjpwYXNz", good, 401, "Bearer"},
		{"unknown token", "Bearer tk_unknownunknownunknownunknownu", good, 401, "does not know"},
		{"wrong topic for token", "Bearer " + testCallerToken, `{"topic":"system","message":"x"}`, 403, "topic 'system'"},
		{"unknown topic", "Bearer " + testCallerToken, `{"topic":"nope","message":"x"}`, 400, "topic must be"},
		{"missing message", "Bearer " + testCallerToken, `{"topic":"general"}`, 400, "message is required"},
		{"bad priority", "Bearer " + testCallerToken, `{"topic":"general","message":"x","priority":7}`, 400, "priority"},
		{"too big", "Bearer " + testCallerToken, `{"topic":"general","message":"` + strings.Repeat("a", 9000) + `"}`, 400, "larger than"},
	}
	for _, c := range cases {
		rec := postNotify(h, c.auth, c.body)
		if rec.Code != c.code {
			t.Errorf("%s: status %d, want %d (%s)", c.name, rec.Code, c.code, rec.Body.String())
			continue
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Errorf("%s: not JSON: %s", c.name, rec.Body.String())
			continue
		}
		if c.code == 200 {
			if out["ok"] != true || out["id"] != "new1" {
				t.Errorf("%s: body %v", c.name, out)
			}
		} else if e, _ := out["error"].(string); !strings.Contains(e, c.errPart) {
			t.Errorf("%s: error %q does not mention %q", c.name, e, c.errPart)
		}
	}

	// Portal's own token is never used to publish.
	f.mu.Lock()
	for _, a := range f.auths {
		if strings.Contains(a, testPortalToken) {
			t.Fatalf("Portal's token was sent on a publish: %q", a)
		}
	}
	if len(f.auths) != 3 { // ok, unknown token, wrong topic: everything else stopped in Portal
		t.Errorf("ntfy saw %d publishes, want 3", len(f.auths))
	}
	f.mu.Unlock()

	// Wrong content type.
	if rec := postNotify(h, "Bearer "+testCallerToken, good, "Content-Type", "text/plain"); rec.Code != 415 {
		t.Errorf("text/plain: %d", rec.Code)
	}
	// GET is not allowed.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/notify", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/notify: %d", rec.Code)
	}

	s.metrics.mu.Lock()
	okCount, forb := s.metrics.notifies["ok"], s.metrics.notifies["forbidden"]
	s.metrics.mu.Unlock()
	if okCount != 1 || forb != 1 {
		t.Errorf("metrics ok=%d forbidden=%d", okCount, forb)
	}
}

func TestNotifyRateLimit(t *testing.T) {
	ntfy := httptest.NewServer(&fakeNtfy{})
	defer ntfy.Close()
	s := newTestServer(t, ntfy.URL)
	h := s.routes()
	body := `{"topic":"general","message":"x"}`
	for i := 0; i < 60; i++ {
		if rec := postNotify(h, "Bearer "+testCallerToken, body, "X-Forwarded-For", "100.64.0.5"); rec.Code != 200 {
			t.Fatalf("request %d: %d", i+1, rec.Code)
		}
	}
	rec := postNotify(h, "Bearer "+testCallerToken, body, "X-Forwarded-For", "100.64.0.5")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("61st request: %d", rec.Code)
	}
	// Even requests without a token count: no free token guessing.
	if rec := postNotify(h, "", body, "X-Forwarded-For", "100.64.0.5"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("tokenless request while limited: %d", rec.Code)
	}
	if rec := postNotify(h, "Bearer "+testCallerToken, body, "X-Forwarded-For", "100.64.0.6"); rec.Code != 200 {
		t.Fatalf("another client was limited: %d", rec.Code)
	}
}

func TestNotifyUnconfigured(t *testing.T) {
	s := newTestServer(t, "")
	rec := postNotify(s.routes(), "Bearer "+testCallerToken, `{"topic":"general","message":"x"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
	html := s.fragment("notify", s.notifView())
	if !strings.Contains(html, "ntfy not reachable") {
		t.Fatalf("panel does not say ntfy is not reachable:\n%s", html)
	}
}

func TestNotifyPanelEscapesAndStyles(t *testing.T) {
	ntfy := httptest.NewServer(&fakeNtfy{})
	defer ntfy.Close()
	s := newTestServer(t, ntfy.URL)
	if _, err := s.notifyPoll.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	html := s.fragment("notify", s.notifView())
	if strings.Contains(html, "<script>") || strings.Contains(html, "<b>hi</b>") {
		t.Fatalf("message content was not escaped:\n%s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") || !strings.Contains(html, `data-prio="urgent"`) {
		t.Fatalf("panel missing escaped text or urgent styling:\n%s", html)
	}
	if strings.Contains(html, "style=") {
		t.Fatal("inline style would break the CSP")
	}
}

func TestNotifyPanelDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	s := newTestServer(t, url)
	s.notifyPoll.Refresh(t.Context())
	if html := s.fragment("notify", s.notifView()); !strings.Contains(html, "ntfy not reachable") {
		t.Fatalf("down state not shown:\n%s", html)
	}
}

func TestBuildNotifHelpers(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now", 5 * time.Minute: "5 min ago", 3 * time.Hour: "3 h ago", 50 * time.Hour: "2 d ago",
	} {
		if got := ago(now.Add(-d), now); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
	long := strings.Repeat("é", panelMessageRunes+10)
	if got := truncateRunes(long, panelMessageRunes); !strings.HasSuffix(got, "…") || len([]rune(got)) != panelMessageRunes+1 {
		t.Errorf("truncate: %d runes", len([]rune(got)))
	}
	d := buildNotif([]notify.Message{{ID: "a", Time: now, Topic: "system", Message: "m", Priority: 1}},
		notify.Status{Configured: true, OK: true, CheckedAt: now}, now)
	if d.State != "ok" || d.Items[0].Prio != "quiet" {
		t.Errorf("buildNotif: %+v", d)
	}
}
