// Package server is Portal's HTTP layer: the forward-auth endpoint that
// Caddy consults, the login pages, the dashboard and its live updates.
package server

import (
	"context"
	"database/sql"
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"portal/internal/apps"
	"portal/internal/auth"
	"portal/internal/docker"
	"portal/internal/metricsdb"
	"portal/internal/notify"
	"portal/internal/procmem"
	"portal/internal/sysinfo"
)

//go:embed web
var webFS embed.FS

// Config is everything the server needs from its environment.
type Config struct {
	Listen       string // address to listen on, loopback only: Caddy is the front door
	Domain       string // e.g. "exp.lab"
	DataDir      string // holds portal.db and apps.yaml
	DockerSock   string // unix socket of the docker-socket-proxy
	User         string // display name of the single user
	CookieSecure bool   // true once the site is served over HTTPS
	NtfyURL      string // e.g. http://127.0.0.1:2586; empty disables notifications
	NtfyToken    string // Portal's READ-ONLY ntfy token, used only for polling
	VMURL        string // VictoriaMetrics, e.g. http://127.0.0.1:8428; empty disables graphs/uptime
	BackupDir    string // shared with the backup container; default <DataDir>/backup
	// HostProc is the host's /proc mounted read-only (e.g. /host/proc) for
	// the Memory panel; empty disables the panel.
	HostProc string
	// ClaudeSessions is Claude Code's ~/.claude/sessions, read-only;
	// optional, it only adds working directories and idle/busy status.
	ClaudeSessions string
	// AlertmanagerURL is where Portal creates and expires silences for
	// containers stopped from the dashboard; empty disables silences.
	AlertmanagerURL string
}

// Scheme is the URL scheme browsers use to reach the site.
func (c Config) Scheme() string {
	if c.CookieSecure {
		return "https"
	}
	return "http"
}

// BaseURL is the dashboard's origin, e.g. "http://exp.lab".
func (c Config) BaseURL() string { return c.Scheme() + "://" + c.Domain }

// cookieName is the session cookie. Caddy strips it from requests before
// they reach the apps (see Caddyfile), so only Portal ever reads it.
const cookieName = "portal_session"

// Server ties the pieces together.
type Server struct {
	cfg      Config
	db       *sql.DB
	sessions *auth.SessionStore
	limiter  *auth.Limiter
	apps     *apps.Registry
	sys      *sysinfo.Collector
	docker   dockerAPI
	vm       *metricsdb.Client // nil when not configured
	hub      *Hub
	metrics  *Metrics
	tmpl     *template.Template

	// Notifications. The poller holds Portal's read-only token; the
	// publisher holds no token at all (callers bring their own).
	notifyPoll  *notify.Poller
	notifyPub   *notify.Publisher
	notifyLimit *notify.RateLimiter
	hostname    string
	version     string // cache-busting suffix for static files

	graphCache *graphCache

	// Backups: the shared directory, and the request rate limit.
	backupDir          string
	backupMu           sync.Mutex // serialises request handling (check, then write)
	backupLastAccepted time.Time

	// Container collector state, only touched with collectMu held.
	collectMu sync.Mutex
	cpuPrev   map[string]docker.CPUSample // last CPU sample per container ID

	// Latest readings, shared between the background loops (writers) and
	// the HTTP handlers (readers).
	mu           sync.RWMutex
	lastSys      sysinfo.Snapshot
	cpuHist      []float64
	containers   []docker.Container
	containersAt time.Time // last successful collection
	dockerErr    string
	uptime       uptimeData
	vmCheckedAt  time.Time // last VictoriaMetrics call (any outcome)
	vmErr        string    // its error, "" if it worked
	alerts       alertsData
	mem          procmem.Breakdown // last host memory scan
	memErr       string

	memScan *procmem.Scanner // nil when PORTAL_HOST_PROC is not set

	silencer *silencer // Alertmanager silences for stopped containers (control.go)
}

// New builds a server. It does not start listening.
func New(cfg Config, db *sql.DB) (*Server, error) {
	reg, err := apps.NewRegistry(filepath.Join(cfg.DataDir, "apps.yaml"), cfg.Domain)
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("").Funcs(template.FuncMap{"icon": iconID}).ParseFS(webFS, "web/templates/*.html")
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	backupDir := cfg.BackupDir
	if backupDir == "" {
		backupDir = filepath.Join(cfg.DataDir, "backup")
	}
	var memScan *procmem.Scanner
	if cfg.HostProc != "" {
		memScan = &procmem.Scanner{Proc: cfg.HostProc, SessionsDir: cfg.ClaudeSessions, UID: os.Getuid()}
	}
	return &Server{
		memScan: memScan,
		cfg:     cfg, db: db,
		sessions: auth.NewSessionStore(db),
		limiter:  auth.NewLimiter(),
		apps:     reg,
		sys:      sysinfo.New(cfg.DataDir),
		docker:   docker.New(cfg.DockerSock),
		vm:       metricsdb.New(cfg.VMURL),
		hub:      NewHub(),
		metrics:  NewMetrics(),
		tmpl:     tmpl,
		hostname: host,
		version:  strconv.FormatInt(time.Now().Unix(), 36),

		notifyPoll:  notify.NewPoller(cfg.NtfyURL, cfg.NtfyToken),
		notifyPub:   notify.NewPublisher(cfg.NtfyURL),
		notifyLimit: notify.NewRateLimiter(60, time.Minute),

		graphCache: newGraphCache(),
		backupDir:  backupDir,
		cpuPrev:    map[string]docker.CPUSample{},
		silencer:   newSilencer(cfg.AlertmanagerURL),
	}, nil
}

// Run serves HTTP until ctx is cancelled, then shuts down cleanly.
func (s *Server) Run(ctx context.Context) error {
	s.startBackground(ctx)

	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
		// No WriteTimeout: the /events stream stays open for hours. That
		// handler sets its own deadline before every write instead.
		MaxHeaderBytes: 32 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("portal listening on %s for %s", s.cfg.Listen, s.cfg.BaseURL())

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		s.hub.CloseAll() // end open event streams so Shutdown can finish
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

// routes is the complete URL map. Go 1.22+ patterns include the method,
// so a POST to a GET-only path is answered with 405 automatically.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Open endpoints.
	mux.Handle("GET /healthz", s.wrap("healthz", s.handleHealthz))
	mux.Handle("GET /login", s.wrap("login", s.handleLoginPage))
	mux.Handle("POST /login", s.wrap("login", s.handleLoginSubmit))
	mux.Handle("POST /logout", s.wrap("logout", s.handleLogout))
	static, _ := fs.Sub(webFS, "web/static")
	mux.Handle("GET /static/", s.wrap("static", cacheStatic(http.StripPrefix("/static/", http.FileServerFS(static))).ServeHTTP))

	// For services, not browsers: no session, but an ntfy token is
	// required and forwarded (see notify.go).
	mux.Handle("POST /api/notify", s.wrap("notify", s.handleNotify))

	// Called by Caddy for every request to an app hostname.
	mux.Handle("GET /auth/verify", s.wrap("verify", s.handleVerify))

	// Session required.
	mux.Handle("GET /{$}", s.wrap("dashboard", s.handleDashboard))
	mux.Handle("GET /events", s.wrap("events", s.handleEvents))
	mux.Handle("GET /api/session", s.wrap("session", s.handleSessionProbe))
	mux.Handle("POST /api/containers/{id}/restart", s.wrap("restart", s.handleRestart))
	mux.Handle("POST /api/containers/{id}/stop", s.wrap("stop", s.handleStop))
	mux.Handle("POST /api/apps/{id}/stop", s.wrap("app_stop", s.handleAppControl("stop")))
	mux.Handle("POST /api/apps/{id}/start", s.wrap("app_start", s.handleAppControl("start")))
	mux.Handle("GET /graphs", s.wrap("graphs", s.handleGraphsPage))
	mux.Handle("GET /api/graphs/{name}", s.wrap("graph_data", s.handleGraphData))
	mux.Handle("POST /api/backup/run", s.wrap("backup_request", s.handleBackupRun))
	mux.Handle("POST /api/backup/restore-staging", s.wrap("backup_request", s.handleBackupRestore))

	// Open on loopback without X-Forwarded-For (the scraper); session
	// otherwise. See handleMetrics.
	mux.Handle("GET /metrics", s.wrap("metrics", s.handleMetrics))
	return s.styledNotFound(mux)
}

// errorPage is the data for error.html.
type errorPage struct {
	V, Title, Text string
	Code           int
}

// styledNotFound answers a browser's GET for an unknown path with a page
// in the site's look (and the usual security headers) instead of Go's
// plain "404 page not found". Everything else, including 405 for a GET to
// a POST-only path, is the mux's own answer.
func (s *Server) styledNotFound(mux *http.ServeMux) http.Handler {
	notFound := s.wrap("not_found", func(w http.ResponseWriter, r *http.Request) {
		s.render(w, http.StatusNotFound, "error.html", errorPage{V: s.version, Code: http.StatusNotFound,
			Title: "Nothing here", Text: "There is no page at this address."})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
			if _, pattern := mux.Handler(r); pattern == "" {
				// No pattern matched: either a 404 or a 405. A 405 lists
				// the allowed methods; ask the mux which it is.
				probe := &headerProbe{h: http.Header{}}
				mux.ServeHTTP(probe, r)
				if probe.code == http.StatusNotFound {
					notFound.ServeHTTP(w, r)
					return
				}
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// headerProbe records only the status code of a response.
type headerProbe struct {
	h    http.Header
	code int
}

func (p *headerProbe) Header() http.Header         { return p.h }
func (p *headerProbe) Write(b []byte) (int, error) { return len(b), nil }
func (p *headerProbe) WriteHeader(code int)        { p.code = code }

// statusRecorder remembers the status code a handler wrote, for metrics.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the real writer, which the
// event stream needs in order to flush.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// wrap is the middleware around every handler: security headers, panic
// recovery and request counting.
func (s *Server) wrap(name string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		// Content-Security-Policy: the page may only load scripts and
		// styles from this origin. Injected <script> tags or inline
		// handlers will not run, which blunts cross-site scripting.
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("X-Frame-Options", "DENY")

		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				log.Printf("panic in %s: %v", name, p)
				http.Error(rec, "internal error", http.StatusInternalServerError)
			}
			s.metrics.Request(name, rec.code)
		}()
		h(rec, r)
	})
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Safe to cache for long: every URL carries ?v=<start time>, so a
		// new deploy changes the URL.
		w.Header().Set("Cache-Control", "public, max-age=604800")
		next.ServeHTTP(w, r)
	})
}

// sessionToken returns the cookie value if it belongs to a live session.
func (s *Server) sessionToken(r *http.Request) (string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", false
	}
	if _, ok := s.sessions.Validate(c.Value); !ok {
		return "", false
	}
	return c.Value, true
}

// setSessionCookie writes (or with value "" clears) the session cookie.
func (s *Server) setSessionCookie(w http.ResponseWriter, value string) {
	c := &http.Cookie{
		Name:  cookieName,
		Value: value,
		// Domain makes the browser send the cookie to exp.lab AND every
		// subdomain. Without it, the cookie would be "host-only" and
		// focal.exp.lab would never see it: no single sign-on.
		Domain: s.cfg.Domain,
		Path:   "/",
		// HttpOnly: JavaScript cannot read it, so a script-injection bug
		// in any app cannot steal the session.
		HttpOnly: true,
		// Lax: sent on top-level navigation (clicking a link to an app)
		// but not on requests fired by other sites' pages.
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.CookieSecure,
		// The browser keeps the cookie for 400 days (the most browsers
		// allow). The real limit is server-side: the 30-day sliding
		// expiry in the sessions table.
		MaxAge: 400 * 24 * 3600,
	}
	if value == "" {
		c.MaxAge = -1 // delete now
	}
	http.SetCookie(w, c)
}

// sameOrigin guards state-changing requests against cross-site request
// forgery. SameSite=Lax already stops other *sites*, but focal.exp.lab
// and exp.lab count as the same site, so a compromised app could still
// post to the dashboard. Browsers always send Origin on POST and pages
// cannot fake it; requiring it to be exactly our origin closes that gap.
func (s *Server) sameOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == s.cfg.BaseURL()
}

// clientIP returns the address of the browser. Portal only listens on
// loopback, so the TCP peer is always Caddy; Caddy appends the real
// client address as the last entry of X-Forwarded-For (and discards
// whatever the client sent in that header itself).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// Healthy means "can answer HTTP and reach the database".
	if err := s.db.PingContext(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

// render executes a named template into a buffer first, so a template
// error produces a clean 500 instead of half a page.
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf strings.Builder
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write([]byte(buf.String()))
}

// fragment renders a template to a string (for the event stream).
func (s *Server) fragment(name string, data any) string {
	var buf strings.Builder
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
	}
	return buf.String()
}

// knownIcons are the symbols defined in web/templates/icons.html.
var knownIcons = map[string]bool{
	"timer": true, "check": true, "git": true, "bot": true, "globe": true, "server": true,
	"database": true, "chart": true, "book": true, "home": true, "code": true, "box": true,
	"film": true, "music": true, "cloud": true, "wallet": true,
}

// iconID maps an apps.yaml icon name to a sprite id, with a fallback so
// a typo shows a generic icon rather than nothing.
func iconID(name string) string {
	if knownIcons[name] {
		return "i-" + name
	}
	return "i-box"
}
