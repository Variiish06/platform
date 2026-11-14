// Package apps loads apps.yaml (the list of applications behind the
// proxy) and health-checks each one.
//
// apps.yaml is the single source of truth: one entry gives an app its
// hostname, its route through Caddy, its auth requirement, its dashboard
// tile and its health check.
package apps

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// App is one entry in apps.yaml.
type App struct {
	ID          string `yaml:"id"`          // hostname label: "focal" -> focal.exp.lab
	Name        string `yaml:"name"`        // shown on the tile
	Description string `yaml:"description"` // one short line under the name
	Icon        string `yaml:"icon"`        // name of a built-in icon (see web/templates/icons.html)
	Upstream    string `yaml:"upstream"`    // host:port the app really listens on
	Auth        *bool  `yaml:"auth"`        // require Portal login? default true
	Health      string `yaml:"health"`      // path to probe, default "/"
	// Containers are the Docker container names that make up the app.
	// Optional. When set, the dashboard row gets a Stop/Start button that
	// acts on exactly these containers (names are matched against the
	// live container list; unknown ones are shown as an error and never
	// sent to Docker).
	Containers []string `yaml:"containers"`
}

// RequiresAuth reports whether the app sits behind Portal's login.
// Auth is a pointer so that "not written in the file" (nil) can default
// to true: forgetting the line must never expose an app.
func (a App) RequiresAuth() bool { return a.Auth == nil || *a.Auth }

// Status is the result of the latest health check of one app.
type Status struct {
	Up        bool
	Code      int           // HTTP status, 0 if the connection failed
	Latency   time.Duration // time to response headers
	CheckedAt time.Time
	Err       string
}

type file struct {
	Apps []App `yaml:"apps"`
}

var (
	idRe       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	upstreamRe = regexp.MustCompile(`^[A-Za-z0-9.-]+:[0-9]{1,5}$`)
	// Docker's own rule for container names.
	containerRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
)

// Registry holds the current app list and health results. It is safe for
// concurrent use: HTTP handlers read while the checker goroutine writes.
type Registry struct {
	path   string
	domain string
	client *http.Client

	mu      sync.RWMutex
	apps    []App
	byHost  map[string]App
	status  map[string]Status
	modTime time.Time
	loadErr string
}

// NewRegistry loads path. domain is the private domain, e.g. "exp.lab".
func NewRegistry(path, domain string) (*Registry, error) {
	r := &Registry{
		path: path, domain: domain, status: map[string]Status{},
		client: &http.Client{
			Timeout: 5 * time.Second,
			// A redirect (e.g. to the app's own login page) already proves
			// the app is alive, so do not follow it.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
				DisableKeepAlives:     true, // one probe every 30s: idle connections would only waste memory
				ResponseHeaderTimeout: 5 * time.Second,
			},
		},
	}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// load parses and validates the file, then swaps it in atomically.
func (r *Registry) load() error {
	info, err := os.Stat(r.path)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return err
	}
	var f file
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("%s: %w", r.path, err)
	}
	byHost := make(map[string]App, len(f.Apps))
	for i := range f.Apps {
		a := &f.Apps[i]
		if !idRe.MatchString(a.ID) {
			return fmt.Errorf("%s: app %d: id %q must be lowercase letters, digits or dashes", r.path, i+1, a.ID)
		}
		if !upstreamRe.MatchString(a.Upstream) {
			return fmt.Errorf("%s: app %q: upstream %q must look like host:port", r.path, a.ID, a.Upstream)
		}
		if a.Name == "" {
			a.Name = a.ID
		}
		if a.Health == "" {
			a.Health = "/"
		}
		if !strings.HasPrefix(a.Health, "/") {
			return fmt.Errorf("%s: app %q: health must start with /", r.path, a.ID)
		}
		seen := map[string]bool{}
		for _, c := range a.Containers {
			if !containerRe.MatchString(c) {
				return fmt.Errorf("%s: app %q: container %q is not a valid container name", r.path, a.ID, c)
			}
			if seen[c] {
				return fmt.Errorf("%s: app %q: container %q is listed twice", r.path, a.ID, c)
			}
			seen[c] = true
		}
		host := a.ID + "." + r.domain
		if _, dup := byHost[host]; dup {
			return fmt.Errorf("%s: duplicate app id %q", r.path, a.ID)
		}
		byHost[host] = *a
	}
	r.mu.Lock()
	r.apps, r.byHost, r.modTime, r.loadErr = f.Apps, byHost, info.ModTime(), ""
	r.mu.Unlock()
	return nil
}

// ReloadIfChanged re-reads apps.yaml when its modification time changed.
// A broken edit keeps the previous good config and reports the error, so
// a typo cannot take every route down. Returns true if the list changed.
func (r *Registry) ReloadIfChanged() (bool, error) {
	info, err := os.Stat(r.path)
	if err != nil {
		return false, err
	}
	r.mu.RLock()
	same := info.ModTime().Equal(r.modTime)
	r.mu.RUnlock()
	if same {
		return false, nil
	}
	if err := r.load(); err != nil {
		r.mu.Lock()
		r.modTime, r.loadErr = info.ModTime(), err.Error() // do not retry until the file changes again
		r.mu.Unlock()
		return false, err
	}
	return true, nil
}

// LoadError returns the error from the last failed reload, or "".
func (r *Registry) LoadError() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.loadErr
}

// List returns the apps in file order.
func (r *Registry) List() []App {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]App(nil), r.apps...)
}

// ByHost finds the app for a Host header value such as "focal.exp.lab".
func (r *Registry) ByHost(host string) (App, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byHost[host]
	return a, ok
}

// ByID finds an app by its id ("focal").
func (r *Registry) ByID(id string) (App, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, a := range r.apps {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}

// Host returns the public hostname of an app.
func (r *Registry) Host(a App) string { return a.ID + "." + r.domain }

// Status returns the latest health result for an app id.
func (r *Registry) Status(id string) (Status, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.status[id]
	return s, ok
}

// CheckAll probes every app at the same time and stores the results.
func (r *Registry) CheckAll(ctx context.Context) {
	list := r.List()
	results := make([]Status, len(list))
	var wg sync.WaitGroup
	for i, a := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = r.check(ctx, a)
		}()
	}
	wg.Wait()
	fresh := make(map[string]Status, len(list))
	for i, a := range list {
		fresh[a.ID] = results[i]
	}
	r.mu.Lock()
	r.status = fresh
	r.mu.Unlock()
}

// check probes the app directly on its upstream port, not through Caddy,
// so the result says "the app is alive", independent of proxy and auth.
func (r *Registry) check(ctx context.Context, a App) Status {
	st := Status{CheckedAt: time.Now()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a.Upstream+a.Health, nil)
	if err != nil {
		st.Err = err.Error()
		return st
	}
	req.Host = r.Host(a) // some apps route or build links by Host header
	req.Header.Set("User-Agent", "portal-healthcheck")
	start := time.Now()
	resp, err := r.client.Do(req)
	st.Latency = time.Since(start)
	if err != nil {
		st.Err = shortErr(err)
		return st
	}
	resp.Body.Close()
	st.Code = resp.StatusCode
	// Anything below 500 means the app answered: a 401 or a 302 to its own
	// login page is a healthy app that simply wants credentials.
	st.Up = resp.StatusCode < 500
	if !st.Up {
		st.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return st
}

// shortErr turns a verbose Go network error into a few words for the tile.
func shortErr(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return "connection refused"
	case strings.Contains(msg, "Timeout"), strings.Contains(msg, "deadline exceeded"), strings.Contains(msg, "timeout"):
		return "timed out"
	case strings.Contains(msg, "no such host"):
		return "host not found"
	}
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		return msg[i+2:]
	}
	return msg
}
