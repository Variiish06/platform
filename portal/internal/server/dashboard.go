package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"portal/internal/docker"
	"portal/internal/sysinfo"
)

// ---- View models: the exact strings the templates show. ----
// Formatting happens here, once, in Go. The first page load and every
// live update go through the same functions, so they can never disagree.

// statView is one cell of the System panel. The JSON names are short
// because this struct is also sent to the browser every few seconds.
type statView struct {
	Key   string  `json:"k"`
	Label string  `json:"l"`
	Value string  `json:"v"`
	Sub   string  `json:"s"`
	Pct   float64 `json:"p"`  // bar fill 0..100, or -1 for "no bar"
	State string  `json:"st"` // "", "warn" or "bad"
	// Hist is the recent CPU history ("3.1,4.0,..."), only set on the CPU
	// cell of a full page load so the sparkline is not empty at first.
	Hist string `json:"-"`
}

type appView struct {
	Name, Desc, Icon, Host, URL string
	State                       string // up, down, unknown
	Detail                      string // "Up · 12 ms", "Down · connection refused"
	Open                        bool   // no Portal login in front of it
}

type appsData struct {
	Tiles       []appView
	Up, Total   int
	ConfigError string
}

type containerView struct {
	ID, Name, Status, State, Mem string
	Running                      bool
}

type containersData struct {
	Rows           []containerView
	Running, Total int
	Error          string
}

type dashboardPage struct {
	V, Host, User string
	Stats         []statView
	Apps          appsData
	Containers    containersData
}

// level turns a number into "", "warn" or "bad" against two thresholds.
func level(v, warn, bad float64) string {
	switch {
	case v >= bad:
		return "bad"
	case v >= warn:
		return "warn"
	}
	return ""
}

func gb(b uint64) float64 { return float64(b) / (1 << 30) }

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(used) / float64(total)
}

func fmtBytes(b uint64) string {
	switch {
	case b == 0:
		return "–"
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", gb(b))
	default:
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	}
}

func fmtUptime(d time.Duration) string {
	days, hours, mins := int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

// buildStats turns a raw snapshot into the System panel's cells.
func buildStats(s sysinfo.Snapshot) []statView {
	out := []statView{
		{Key: "cpu", Label: "CPU", Value: fmt.Sprintf("%.0f%%", s.CPUPercent),
			Sub: fmt.Sprintf("load %.2f · %d cores", s.Load1, s.Cores),
			Pct: s.CPUPercent, State: level(s.CPUPercent, 80, 95)},
		{Key: "mem", Label: "Memory", Value: fmt.Sprintf("%.1f / %.1f GB", gb(s.MemUsed), gb(s.MemTotal)),
			Sub: fmt.Sprintf("%.0f%% used", pct(s.MemUsed, s.MemTotal)),
			Pct: pct(s.MemUsed, s.MemTotal), State: level(pct(s.MemUsed, s.MemTotal), 85, 95)},
		{Key: "disk", Label: "Disk", Value: fmt.Sprintf("%.0f / %.0f GB", gb(s.DiskUsed), gb(s.DiskTotal)),
			Sub: fmt.Sprintf("%.0f%% used", pct(s.DiskUsed, s.DiskTotal)),
			Pct: pct(s.DiskUsed, s.DiskTotal), State: level(pct(s.DiskUsed, s.DiskTotal), 80, 92)},
	}

	temp := statView{Key: "temp", Label: "CPU temp", Value: "–", Sub: "no sensor", Pct: -1}
	if s.TempOK {
		temp.Value, temp.Sub = fmt.Sprintf("%.0f°C", s.TempC), "normal"
		temp.Pct, temp.State = s.TempC, level(s.TempC, 75, 90)
		switch temp.State {
		case "warn":
			temp.Sub = "running warm"
		case "bad":
			temp.Sub = "too hot"
		}
	}
	out = append(out, temp)

	bat := statView{Key: "battery", Label: "Battery", Value: "–", Sub: "no battery", Pct: -1}
	if s.BatteryOK {
		bat.Value, bat.Pct = fmt.Sprintf("%d%%", s.BatteryPercent), float64(s.BatteryPercent)
		power := "on battery"
		if s.ACOnline {
			power = "plugged in"
		}
		bat.Sub = s.BatteryStatus + " · " + power
		// A server on battery means the charger fell out or the power is
		// off: worth a warning long before the charge is actually low.
		if !s.ACOnline {
			bat.State = "warn"
			if s.BatteryPercent < 20 {
				bat.State = "bad"
			}
		}
	}
	out = append(out, bat)

	wifi := statView{Key: "wifi", Label: "WiFi", Value: "–", Sub: "not connected", Pct: -1}
	if s.WiFiOK {
		quality := "Weak"
		switch {
		case s.WiFiDBm >= -55:
			quality = "Excellent"
		case s.WiFiDBm >= -67:
			quality = "Good"
		case s.WiFiDBm >= -75:
			quality = "Fair"
		}
		wifi.Value, wifi.Sub = fmt.Sprintf("%d dBm", s.WiFiDBm), quality+" · "+s.WiFiIface
		wifi.Pct = 100 * float64(s.WiFiQuality) / 70 // the kernel reports link quality out of 70
		// dBm is negative: closer to zero is stronger.
		wifi.State = level(float64(-s.WiFiDBm), 72, 82)
	}
	out = append(out, wifi)

	out = append(out, statView{Key: "uptime", Label: "Uptime", Value: fmtUptime(s.Uptime),
		Sub: "since " + s.At.Add(-s.Uptime).Format("2 Jan"), Pct: -1})
	return out
}

func (s *Server) appsView() appsData {
	d := appsData{ConfigError: s.apps.LoadError()}
	for _, a := range s.apps.List() {
		host := s.apps.Host(a)
		v := appView{Name: a.Name, Desc: a.Description, Icon: a.Icon, Host: host,
			URL: s.cfg.Scheme() + "://" + host + "/", State: "unknown", Detail: "Checking", Open: !a.RequiresAuth()}
		if st, ok := s.apps.Status(a.ID); ok {
			if st.Up {
				v.State = "up"
				v.Detail = fmt.Sprintf("Up · %d ms", max(1, st.Latency.Milliseconds()))
				d.Up++
			} else {
				v.State, v.Detail = "down", "Down · "+st.Err
			}
		}
		d.Tiles = append(d.Tiles, v)
	}
	d.Total = len(d.Tiles)
	return d
}

func (s *Server) containersView() containersData {
	s.mu.RLock()
	list, errText := s.containers, s.dockerErr
	s.mu.RUnlock()
	d := containersData{Error: errText, Total: len(list)}
	for _, c := range list {
		if c.Running() {
			d.Running++
		}
		d.Rows = append(d.Rows, containerView{ID: c.ID, Name: c.Name, Status: c.Status, State: c.State,
			Mem: fmtBytes(c.MemBytes), Running: c.Running()})
	}
	return d
}

// ---- Handlers ----

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionToken(r); !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(s.cfg.BaseURL()+"/"), http.StatusFound)
		return
	}
	// The container list is only refreshed while someone is watching, so
	// the first visitor after a quiet period triggers a refresh.
	s.mu.RLock()
	stale := time.Since(s.containersAt) > 15*time.Second
	sys := s.lastSys
	s.mu.RUnlock()
	if stale {
		s.refreshContainers(r.Context())
	}
	stats := buildStats(sys)
	s.mu.RLock()
	hist := make([]string, len(s.cpuHist))
	for i, v := range s.cpuHist {
		hist[i] = fmt.Sprintf("%.1f", v)
	}
	s.mu.RUnlock()
	stats[0].Hist = strings.Join(hist, ",") // stats[0] is the CPU cell
	s.render(w, http.StatusOK, "dashboard.html", dashboardPage{
		V: s.version, Host: s.hostname, User: s.cfg.User,
		Stats: stats, Apps: s.appsView(), Containers: s.containersView(),
	})
}

// handleEvents is the Server-Sent Events stream.
//
// SSE is ordinary HTTP: the server answers a GET with Content-Type
// text/event-stream and then simply never finishes the response. Each
// update is a few lines of text followed by a blank line. The browser's
// built-in EventSource parses them and reconnects by itself if the
// connection drops. The data only flows server -> browser, which is all
// a dashboard needs; WebSockets would add a second protocol for nothing.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	token, ok := s.sessionToken(r)
	if !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // ask any proxy in between not to buffer

	ch, backlog, cancel := s.hub.Subscribe()
	defer cancel()

	// send writes one message and pushes it onto the wire. Without Flush
	// the bytes would sit in a buffer and the page would never update.
	send := func(msg string) bool {
		rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := w.Write([]byte(msg)); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	// "retry:" tells the browser how long to wait before reconnecting.
	if !send("retry: 3000\n\n") {
		return
	}
	for _, m := range backlog {
		if !send(m) {
			return
		}
	}

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done(): // browser went away
			return
		case msg, open := <-ch:
			if !open { // server is shutting down
				return
			}
			if !send(msg) {
				return
			}
		case <-heartbeat.C:
			// A logout elsewhere must end this stream too.
			if _, ok := s.sessions.Validate(token); !ok {
				return
			}
			// Lines starting with ":" are comments. They keep idle
			// connections from being dropped by proxies and phones.
			if !send(": ping\n\n") {
				return
			}
		}
	}
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionToken(r); !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	// Only IDs from our own list are accepted, so the URL sent to Docker
	// is never built from arbitrary input.
	name := ""
	s.mu.RLock()
	for _, c := range s.containers {
		if c.ID == id {
			name = c.Name
		}
	}
	s.mu.RUnlock()
	if name == "" || !docker.ValidID(id) {
		http.Error(w, "unknown container", http.StatusNotFound)
		return
	}
	log.Printf("restart %s requested by %s", name, clientIP(r))
	// Detached from the request: if the browser disconnects (or we are
	// restarting Caddy itself), the restart still completes.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
	defer cancel()
	if err := s.docker.Restart(ctx, id); err != nil {
		log.Printf("restart %s: %v", name, err)
		http.Error(w, "Docker refused the restart", http.StatusBadGateway)
		return
	}
	s.refreshContainers(ctx)
	s.publishContainers()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"restarted": name})
}

// ---- Background work ----

func (s *Server) refreshContainers(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	list, err := s.docker.List(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.containersAt = time.Now()
	if err != nil {
		s.dockerErr = "Can't reach Docker. Is the socket proxy running?"
		log.Printf("docker list: %v", err)
		return
	}
	s.containers, s.dockerErr = list, ""
}

func (s *Server) sampleSystem() {
	snap := s.sys.Snapshot()
	s.mu.Lock()
	s.lastSys = snap
	// Keep the last 60 readings for the sparkline.
	s.cpuHist = append(s.cpuHist, snap.CPUPercent)
	if len(s.cpuHist) > 60 {
		s.cpuHist = s.cpuHist[len(s.cpuHist)-60:]
	}
	s.mu.Unlock()
}

func (s *Server) publishSystem() {
	s.mu.RLock()
	snap := s.lastSys
	s.mu.RUnlock()
	b, _ := json.Marshal(buildStats(snap))
	s.hub.Publish("system", string(b))
}

func (s *Server) publishApps() {
	s.hub.Publish("apps", strings.TrimSpace(s.fragment("apps", s.appsView())))
}

func (s *Server) publishContainers() {
	s.hub.Publish("containers", strings.TrimSpace(s.fragment("containers", s.containersView())))
}

// startBackground launches the loops that keep the dashboard current.
// Each runs in its own goroutine and stops when ctx is cancelled.
func (s *Server) startBackground(ctx context.Context) {
	// Prime everything once so the first page load has data.
	s.sampleSystem()
	go func() {
		s.apps.CheckAll(ctx)
		s.publishApps()
	}()

	every := func(d time.Duration, fn func()) {
		go func() {
			t := time.NewTicker(d)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					fn()
				}
			}
		}()
	}

	// Health checks: every 30s, always (they also feed /metrics).
	every(30*time.Second, func() {
		if _, err := s.apps.ReloadIfChanged(); err != nil {
			log.Printf("apps.yaml: %v (keeping the previous config)", err)
		}
		s.apps.CheckAll(ctx)
		s.publishApps()
	})

	// System stats: every 2s while a dashboard is open, otherwise every
	// 30s, so an unwatched server does almost no work.
	every(2*time.Second, func() {
		s.mu.RLock()
		age := time.Since(s.lastSys.At)
		s.mu.RUnlock()
		if s.hub.Count() == 0 && age < 30*time.Second {
			return
		}
		s.sampleSystem()
		s.publishSystem()
	})

	// Containers: every 10s, only while a dashboard is open.
	every(10*time.Second, func() {
		if s.hub.Count() == 0 {
			return
		}
		s.refreshContainers(ctx)
		s.publishContainers()
	})

	// Housekeeping: drop expired sessions and stale rate-limit entries.
	every(time.Hour, func() {
		if n, err := s.sessions.PurgeExpired(); err == nil && n > 0 {
			log.Printf("purged %d expired sessions", n)
		}
		s.limiter.Cleanup()
	})
}
