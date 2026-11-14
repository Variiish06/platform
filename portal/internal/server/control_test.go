package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"portal/internal/apps"
	"portal/internal/docker"
)

// ---- fake Alertmanager ----

type fakeAM struct {
	mu       sync.Mutex
	silences []amSilence
	comments map[string]string
	expired  []string // deleted
	wound    []string // end moved to a few minutes from now
	n        int
}

func (f *fakeAM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/v2/silences":
		var in struct {
			ID        string      `json:"id"`
			Matchers  []amMatcher `json:"matchers"`
			CreatedBy string      `json:"createdBy"`
			Comment   string      `json:"comment"`
			StartsAt  time.Time   `json:"startsAt"`
			EndsAt    time.Time   `json:"endsAt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Matchers) == 0 {
			http.Error(w, "bad silence", 400)
			return
		}
		if d := in.EndsAt.Sub(in.StartsAt); in.ID == "" && (d < 29*24*time.Hour || d > 31*24*time.Hour) {
			http.Error(w, "unexpected duration "+d.String(), 400)
			return
		}
		if in.ID != "" { // update: Portal winds a silence down after a start
			for i := range f.silences {
				if f.silences[i].ID == in.ID {
					if d := time.Until(in.EndsAt); d <= 0 || d > silenceGrace+time.Second {
						http.Error(w, "unexpected wind-down "+d.String(), 400)
						return
					}
					f.silences[i].EndsAt = in.EndsAt
					f.wound = append(f.wound, in.ID)
					json.NewEncoder(w).Encode(map[string]string{"silenceID": in.ID})
					return
				}
			}
			http.Error(w, "no such silence", 404)
			return
		}
		f.n++
		sl := amSilence{ID: "0000000" + string(rune('0'+f.n)) + "-aaaa-bbbb-cccc-dddddddddddd", CreatedBy: in.CreatedBy, Matchers: in.Matchers,
			StartsAt: in.StartsAt, EndsAt: in.EndsAt, Comment: in.Comment}
		sl.Status.State = "active"
		f.silences = append(f.silences, sl)
		if f.comments == nil {
			f.comments = map[string]string{}
		}
		f.comments[sl.ID] = in.Comment
		json.NewEncoder(w).Encode(map[string]string{"silenceID": sl.ID})
	case r.Method == "GET" && r.URL.Path == "/api/v2/silences":
		json.NewEncoder(w).Encode(f.silences)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/v2/silence/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v2/silence/")
		for i := range f.silences {
			if f.silences[i].ID == id {
				f.silences[i].Status.State = "expired"
				f.expired = append(f.expired, id)
				return
			}
		}
		http.Error(w, "not found", 404)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 400)
	}
}

// active returns "label=value" for each active silence that is not
// winding down.
func (f *fakeAM) active() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.silences {
		if s.Status.State == "active" && time.Until(s.EndsAt) > time.Hour {
			out = append(out, s.Matchers[0].Name+"="+s.Matchers[0].Value)
		}
	}
	slices.Sort(out)
	return out
}

// ---- setup ----

const ctlOrigin = "http://exp.lab"

var ctlContainers = []docker.Container{
	{ID: id(1), Name: "focal", Project: "focal", Service: "focal", State: "running"},
	{ID: id(2), Name: "platform-portal-1", Project: "platform", Service: "portal", State: "running"},
	{ID: id(3), Name: "groqbots-orchestrator-1", Project: "groqbots", Service: "orchestrator", State: "running"},
	{ID: id(4), Name: "groqbots-testsite-1", Project: "groqbots", Service: "testsite", State: "running"},
	// "portal" service, but not of the platform project: not protected.
	{ID: id(5), Name: "other-portal-1", Project: "other", Service: "portal", State: "running"},
	{ID: id(6), Name: "platform-caddy-1", Project: "platform", Service: "caddy", State: "running"},
}

const ctlAppsYAML = `apps:
  - id: bot
    name: Bots
    upstream: 127.0.0.1:1
    containers: [groqbots-orchestrator-1]
  - id: ghost
    name: Ghost
    upstream: 127.0.0.1:1
    containers: [no-such-container]
  - id: sneaky
    name: Sneaky
    upstream: 127.0.0.1:1
    containers: [platform-caddy-1]
  - id: plain
    name: Plain
    upstream: 127.0.0.1:1
`

func newCtlServer(t *testing.T, amURL string) (*Server, *fakeDocker, string) {
	t.Helper()
	s, token := newPhaseBServer(t, "")
	p := filepath.Join(t.TempDir(), "apps.yaml")
	if err := os.WriteFile(p, []byte(ctlAppsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := apps.NewRegistry(p, "exp.lab")
	if err != nil {
		t.Fatal(err)
	}
	s.apps = reg
	f := &fakeDocker{list: append([]docker.Container(nil), ctlContainers...)}
	s.docker = f
	s.silencer = newSilencer(amURL)
	s.collectContainers(context.Background())
	return s, f, token
}

func state(s *Server, name string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.containers {
		if c.Name == name {
			return c.State
		}
	}
	return "?"
}

// ---- tests ----

func TestStopContainer(t *testing.T) {
	am := &fakeAM{}
	srv := httptest.NewServer(am)
	defer srv.Close()
	s, f, token := newCtlServer(t, srv.URL)
	h := s.routes()

	rec := do(h, "POST", "/api/containers/"+id(1)+"/stop", token, ctlOrigin, "")
	if rec.Code != 200 {
		t.Fatalf("stop: %d %s", rec.Code, rec.Body)
	}
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["stopped"] != "focal" || out["warning"] != "" {
		t.Errorf("response %v", out)
	}
	if !slices.Equal(f.stopped, []string{id(1)}) {
		t.Errorf("stopped %v", f.stopped)
	}
	if state(s, "focal") != "exited" {
		t.Errorf("list not refreshed: %s", state(s, "focal"))
	}
	if got := am.active(); !slices.Equal(got, []string{"name=focal"}) {
		t.Errorf("silences %v", got)
	}
	for _, c := range am.comments {
		if c != "stopped from Portal by "+s.cfg.User {
			t.Errorf("comment %q", c)
		}
	}
	// The Containers table now offers Start for it.
	html := s.fragment("containers", s.containersView())
	if !strings.Contains(html, `data-ctl="start" data-kind="container" data-target="`+id(1)+`"`) {
		t.Error("no Start button for the stopped container")
	}

	// Starting it again (restart endpoint) expires the silence.
	if rec := do(h, "POST", "/api/containers/"+id(1)+"/restart", token, ctlOrigin, ""); rec.Code != 200 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	if state(s, "focal") != "running" {
		t.Error("not started")
	}
	if got := am.active(); len(got) != 0 {
		t.Errorf("silence not expired: %v", got)
	}
}

func TestStopRefusals(t *testing.T) {
	s, f, token := newCtlServer(t, "")
	h := s.routes()
	cases := []struct {
		name, path, token, origin string
		want                      int
	}{
		{"unknown id", "/api/containers/" + id(99) + "/stop", token, ctlOrigin, 404},
		{"not an id", "/api/containers/focal/stop", token, ctlOrigin, 404},
		{"protected portal", "/api/containers/" + id(2) + "/stop", token, ctlOrigin, 403},
		{"protected caddy", "/api/containers/" + id(6) + "/stop", token, ctlOrigin, 403},
		{"cross-origin sibling app", "/api/containers/" + id(1) + "/stop", token, "http://focal.exp.lab", 403},
		{"no origin", "/api/containers/" + id(1) + "/stop", token, "", 403},
		{"no session", "/api/containers/" + id(1) + "/stop", "", ctlOrigin, 401},
		{"bad session", "/api/containers/" + id(1) + "/stop", "nope", ctlOrigin, 401},
		{"app no session", "/api/apps/bot/stop", "", ctlOrigin, 401},
		{"app cross-origin", "/api/apps/bot/stop", token, "http://evil.example", 403},
		{"app unknown", "/api/apps/nope/stop", token, ctlOrigin, 404},
		{"app without containers", "/api/apps/plain/stop", token, ctlOrigin, 404},
		{"app unknown container", "/api/apps/ghost/stop", token, ctlOrigin, 409},
		{"app protected container", "/api/apps/sneaky/stop", token, ctlOrigin, 409},
		{"GET not allowed", "/api/containers/" + id(1) + "/stop", token, ctlOrigin, 405},
	}
	for _, c := range cases {
		method := "POST"
		if strings.HasPrefix(c.name, "GET") {
			method = "GET"
		}
		if rec := do(h, method, c.path, c.token, c.origin, ""); rec.Code != c.want {
			t.Errorf("%s: %d, want %d (%s)", c.name, rec.Code, c.want, strings.TrimSpace(rec.Body.String()))
		}
	}
	if len(f.stopped)+len(f.restarted) != 0 {
		t.Fatalf("Docker was called: stopped %v restarted %v", f.stopped, f.restarted)
	}
	// Protection is by compose labels: a "portal" service of another
	// project can be stopped.
	if rec := do(h, "POST", "/api/containers/"+id(5)+"/stop", token, ctlOrigin, ""); rec.Code != 200 {
		t.Errorf("other-portal-1: %d", rec.Code)
	}
	// Restarting a protected container is still allowed.
	if rec := do(h, "POST", "/api/containers/"+id(2)+"/restart", token, ctlOrigin, ""); rec.Code != 200 {
		t.Errorf("restart portal: %d", rec.Code)
	}
}

func TestContainerButtons(t *testing.T) {
	s, _, _ := newCtlServer(t, "")
	html := s.fragment("containers", s.containersView())
	btn := func(action, i string) string {
		return `data-ctl="` + action + `" data-kind="container" data-target="` + i + `"`
	}
	for _, want := range []string{btn("restart", id(1)), btn("stop", id(1)), btn("restart", id(2)), btn("stop", id(5))} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, bad := range []string{btn("stop", id(2)), btn("stop", id(6))} {
		if strings.Contains(html, bad) {
			t.Errorf("protected container has a Stop button: %s", bad)
		}
	}
}

func TestAppStopStart(t *testing.T) {
	am := &fakeAM{}
	srv := httptest.NewServer(am)
	defer srv.Close()
	s, f, token := newCtlServer(t, srv.URL)
	h := s.routes()

	// The row offers Stop, and errors for misconfigured apps.
	view := map[string]appView{}
	for _, a := range s.appsView().Tiles {
		view[a.ID] = a
	}
	if view["bot"].Ctl != "stop" || view["bot"].CtlNames != "groqbots-orchestrator-1" {
		t.Errorf("bot row %+v", view["bot"])
	}
	if view["ghost"].Ctl != "" || !strings.Contains(view["ghost"].CtlError, "no-such-container") {
		t.Errorf("ghost row %+v", view["ghost"])
	}
	if view["sneaky"].Ctl != "" || !strings.Contains(view["sneaky"].CtlError, "platform-caddy-1") {
		t.Errorf("sneaky row %+v", view["sneaky"])
	}
	if view["plain"].Ctl != "" || view["plain"].CtlError != "" {
		t.Errorf("plain row %+v", view["plain"])
	}
	html := s.fragment("apps", s.appsView())
	if !strings.Contains(html, `data-ctl="stop" data-kind="app" data-target="bot"`) {
		t.Error("no Stop button on the bot row")
	}

	rec := do(h, "POST", "/api/apps/bot/stop", token, ctlOrigin, "")
	if rec.Code != 200 {
		t.Fatalf("app stop: %d %s", rec.Code, rec.Body)
	}
	// Exactly the listed container: not the testsite of the same project.
	if !slices.Equal(f.stopped, []string{id(3)}) {
		t.Fatalf("stopped %v", f.stopped)
	}
	if state(s, "groqbots-testsite-1") != "running" {
		t.Error("testsite was touched")
	}
	if got := am.active(); !slices.Equal(got, []string{"app=bot", "name=groqbots-orchestrator-1"}) {
		t.Errorf("silences %v", got)
	}
	for _, a := range s.appsView().Tiles {
		if a.ID == "bot" && (a.Ctl != "start" || a.State != "stopped") {
			t.Errorf("after stop: %+v", a)
		}
	}
	// The status sentence counts its alert apart, in plain ink.
	st := buildStatus(statusInput{Apps: s.appsView(), AlertsOn: true, Silenced: s.silencedSubjects(),
		Alerts: alertsData{At: time.Now(), List: []firingAlert{{Name: "ContainerDown", Severity: "urgent", Subject: "groqbots-orchestrator-1"}}}})
	if strings.Contains(st.Plain, "ContainerDown") || !strings.Contains(st.Plain, "1 alert silenced") || !strings.Contains(st.Plain, "Bots stopped") {
		t.Errorf("status %q", st.Plain)
	}

	// A second stop is a no-op for Docker.
	do(h, "POST", "/api/apps/bot/stop", token, ctlOrigin, "")
	if len(f.stopped) != 1 {
		t.Errorf("already stopped container stopped again: %v", f.stopped)
	}

	if rec := do(h, "POST", "/api/apps/bot/start", token, ctlOrigin, ""); rec.Code != 200 {
		t.Fatalf("app start: %d %s", rec.Code, rec.Body)
	}
	if !slices.Equal(f.restarted, []string{id(3)}) {
		t.Errorf("restarted %v", f.restarted)
	}
	if got := am.active(); len(got) != 0 {
		t.Errorf("silences left: %v", got)
	}
	// Wound down (a few minutes' grace for the stale alert), not deleted.
	if len(am.wound) != 2 || len(am.expired) != 0 {
		t.Errorf("wound %v expired %v", am.wound, am.expired)
	}
	if subj := s.silencedSubjects(); len(subj) != 0 {
		t.Errorf("still counted as silenced: %v", subj)
	}
}

// After a Portal restart the in-memory ids are gone; starting must still
// find and expire Portal's silences, and leave other people's alone.
func TestUnsilenceFindsOldSilences(t *testing.T) {
	am := &fakeAM{}
	srv := httptest.NewServer(am)
	defer srv.Close()
	s, f, token := newCtlServer(t, srv.URL)
	h := s.routes()
	do(h, "POST", "/api/containers/"+id(1)+"/stop", token, ctlOrigin, "")
	// Someone else's silence on the same matcher.
	am.mu.Lock()
	other := amSilence{ID: "99999999-aaaa-bbbb-cccc-dddddddddddd", CreatedBy: "kunchu", Matchers: []amMatcher{{Name: "name", Value: "focal", IsEqual: true}},
		StartsAt: time.Now(), EndsAt: time.Now().Add(48 * time.Hour)}
	other.Status.State = "active"
	am.silences = append(am.silences, other)
	am.mu.Unlock()

	s.silencer = newSilencer(srv.URL) // "Portal restarted"
	if err := s.silencer.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !s.silencedSubjects()["focal"] {
		t.Error("Load did not pick up Portal's silence")
	}
	if rec := do(h, "POST", "/api/containers/"+id(1)+"/restart", token, ctlOrigin, ""); rec.Code != 200 {
		t.Fatalf("start: %d", rec.Code)
	}
	if !slices.Equal(f.restarted, []string{id(1)}) {
		t.Errorf("restarted %v", f.restarted)
	}
	if got := am.active(); !slices.Equal(got, []string{"name=focal"}) || len(am.wound) != 1 || len(am.expired) != 0 {
		t.Errorf("active %v wound %v expired %v (the other user's silence must stay)", got, am.wound, am.expired)
	}
}

func TestStopWithAlertmanagerDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens there now
	s, f, token := newCtlServer(t, url)
	h := s.routes()
	start := time.Now()
	rec := do(h, "POST", "/api/apps/bot/stop", token, ctlOrigin, "")
	if rec.Code != 200 {
		t.Fatalf("stop failed because Alertmanager is down: %d %s", rec.Code, rec.Body)
	}
	if time.Since(start) > 15*time.Second {
		t.Error("stop waited too long for Alertmanager")
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if w, _ := out["warning"].(string); !strings.Contains(w, "Alertmanager") {
		t.Errorf("no warning: %v", out)
	}
	if !slices.Equal(f.stopped, []string{id(3)}) {
		t.Errorf("stopped %v", f.stopped)
	}
	// Start works too, with a warning.
	rec = do(h, "POST", "/api/apps/bot/start", token, ctlOrigin, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Alertmanager") {
		t.Errorf("start: %d %s", rec.Code, rec.Body)
	}
	// And a Docker failure is reported as such.
	f.stopErr = context.DeadlineExceeded
	if rec := do(h, "POST", "/api/containers/"+id(1)+"/stop", token, ctlOrigin, ""); rec.Code != 502 {
		t.Errorf("docker error: %d", rec.Code)
	}
}

func TestAppsYAMLContainersValidation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "apps.yaml")
	for _, bad := range []string{
		"apps:\n  - id: a\n    upstream: 127.0.0.1:1\n    containers: [\"../x\"]\n",
		"apps:\n  - id: a\n    upstream: 127.0.0.1:1\n    containers: [\"a b\"]\n",
		"apps:\n  - id: a\n    upstream: 127.0.0.1:1\n    containers: [x, x]\n",
	} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := apps.NewRegistry(p, "exp.lab"); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
