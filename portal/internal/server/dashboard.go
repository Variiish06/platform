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

// statView is one reading of the System row. The JSON names are short
// because this struct is also sent to the browser every few seconds.
type statView struct {
	Key   string  `json:"k"`
	Label string  `json:"l"`
	Value string  `json:"v"`
	Sub   string  `json:"s"`
	Pct   float64 `json:"p"`  // meter fill 0..100, or -1 for "no meter"
	State string  `json:"st"` // "", "warn" or "bad"
	// Hist is the recent CPU history ("3.1,4.0,..."), only set on the CPU
	// reading of a full page load so the sparkline is not empty at first.
	Hist string `json:"-"`
}

type appView struct {
	Name, Desc, Icon, Host, URL string
	State                       string // up, down, unknown
	Detail                      string // "connection refused" when down, "checking" when unknown
	Latency                     string // "5 ms" when up
	Open                        bool   // no Portal login in front of it
	Uptime                      uptimeView
}

type appsData struct {
	Span        string // what the uptime history covers, from the first app with data
	Tiles       []appView
	Up, Total   int
	ConfigError string
}

type containerView struct {
	ID, Name, Project, State string
	Status                   string // Docker's own words, for the tooltip
	Uptime                   string // "2 days", "33 hours"; for stopped ones "exited 6 min ago"
	Health                   string // "unhealthy", "starting" or ""
	Mem                      string // "46.0 MiB" or "–"
	MemBytes                 uint64
	MemPct                   string // bar width relative to the largest container
	CPU                      string // "3.2%" or "" until two samples exist
	CPUVal                   float64
	Running                  bool
}

type containersData struct {
	Rows           []containerView
	Running, Total int
	Projects       int
	MemTotal       string
	Error          string
}

type headerData struct {
	Host, Uptime, Since, Time, TimeISO string
}

type dashboardPage struct {
	V, User    string
	Page       string // "dashboard": marks the current link in the strip
	Header     headerData
	Status     statusView
	Stats      []statView
	Apps       appsData
	Memory     memView
	Containers containersData
	Notify     notifData
	Backup     backupData
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
	if b == 0 {
		return "–"
	}
	return fmtMem(int64(b))
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

// buildStats turns a raw snapshot into the System readings.
func buildStats(s sysinfo.Snapshot) []statView {
	memPct, diskPct := pct(s.MemUsed, s.MemTotal), pct(s.DiskUsed, s.DiskTotal)
	out := []statView{
		{Key: "cpu", Label: "CPU", Value: fmt.Sprintf("%.0f%%", s.CPUPercent),
			Sub: fmt.Sprintf("%d cores", s.Cores), Pct: -1, State: level(s.CPUPercent, 80, 95)},
		{Key: "load", Label: "Load 1/5/15", Value: fmt.Sprintf("%.2f", s.Load1),
			Sub: fmt.Sprintf("%.2f  %.2f", s.Load5, s.Load15), Pct: -1,
			State: level(s.Load1/float64(max(1, s.Cores)), 1.5, 3)},
		{Key: "mem", Label: "Memory", Value: fmt.Sprintf("%.1f GiB", gb(s.MemUsed)),
			Sub: fmt.Sprintf("of %.1f · %.0f%%", gb(s.MemTotal), memPct),
			Pct: memPct, State: level(memPct, 90, 97)},
		{Key: "disk", Label: "Disk", Value: fmt.Sprintf("%.0f GiB", gb(s.DiskUsed)),
			Sub: fmt.Sprintf("of %.0f · %.0f%%", gb(s.DiskTotal), diskPct),
			Pct: diskPct, State: level(diskPct, 80, 92)},
	}

	temp := statView{Key: "temp", Label: "Temp", Value: "–", Sub: "no sensor", Pct: -1}
	if s.TempOK {
		temp.Value, temp.Sub = fmt.Sprintf("%.0f °C", s.TempC), "CPU package"
		temp.State = level(s.TempC, 75, 90)
		switch temp.State {
		case "warn":
			temp.Sub = "running warm"
		case "bad":
			temp.Sub = "too hot"
		}
	}
	out = append(out, temp)

	bat := statView{Key: "power", Label: "Power", Value: "–", Sub: "no battery", Pct: -1}
	if s.BatteryOK {
		bat.Value = fmt.Sprintf("%d%%", s.BatteryPercent)
		bat.Sub = "on AC · " + strings.ToLower(s.BatteryStatus)
		// A server on battery means the charger fell out or the power is
		// off: worth a warning long before the charge is actually low.
		if !s.ACOnline {
			bat.Sub, bat.State = "ON BATTERY", "warn"
			if s.BatteryPercent < 20 {
				bat.State = "bad"
			}
		}
	}
	out = append(out, bat)

	wifi := statView{Key: "wifi", Label: "WiFi", Value: "–", Sub: "not connected", Pct: -1}
	if s.WiFiOK {
		quality := "weak"
		switch {
		case s.WiFiDBm >= -55:
			quality = "excellent"
		case s.WiFiDBm >= -67:
			quality = "good"
		case s.WiFiDBm >= -75:
			quality = "fair"
		}
		wifi.Value, wifi.Sub = fmt.Sprintf("%d dBm", s.WiFiDBm), quality
		// dBm is negative: closer to zero is stronger.
		wifi.State = level(float64(-s.WiFiDBm), 72, 82)
	}
	out = append(out, wifi)

	// Shown in the header strip, patched by the same JSON.
	out = append(out, statView{Key: "uptime", Label: "Up", Value: fmtUptime(s.Uptime),
		Sub: "since " + s.At.Add(-s.Uptime).Format("2 Jan"), Pct: -1})
	return out
}

func (s *Server) appsView() appsData {
	d := appsData{ConfigError: s.apps.LoadError()}
	for _, a := range s.apps.List() {
		host := s.apps.Host(a)
		v := appView{Name: a.Name, Desc: a.Description, Icon: a.Icon, Host: host,
			URL: s.cfg.Scheme() + "://" + host + "/", State: "unknown", Detail: "checking", Open: !a.RequiresAuth()}
		if st, ok := s.apps.Status(a.ID); ok {
			if st.Up {
				v.State, v.Detail = "up", ""
				v.Latency = fmt.Sprintf("%d ms", max(1, st.Latency.Milliseconds()))
				d.Up++
			} else {
				v.State, v.Detail = "down", st.Err
			}
		}
		v.Uptime = s.uptimeFor(a.ID)
		if d.Span == "" {
			d.Span = v.Uptime.Span
		}
		d.Tiles = append(d.Tiles, v)
	}
	d.Total = len(d.Tiles)
	return d
}

// containerUptime turns Docker's status text into the table's columns:
// "Up 2 days (healthy)" -> ("2 days", ""), "Up 3 hours (unhealthy)" ->
// ("3 hours", "unhealthy"), "Exited (0) 6 minutes ago" -> ("exited 6 min
// ago", "").
func containerUptime(status string) (uptime, health string) {
	st := strings.TrimSpace(status)
	for _, h := range []string{"unhealthy", "health: starting"} {
		if strings.Contains(st, "("+h+")") {
			health = strings.TrimPrefix(h, "health: ")
		}
	}
	if i := strings.Index(st, " ("); i >= 0 && strings.HasPrefix(st, "Up ") {
		st = st[:i]
	}
	switch {
	case strings.HasPrefix(st, "Up "):
		uptime = strings.TrimPrefix(st, "Up ")
	case strings.HasPrefix(st, "Exited"):
		if i := strings.Index(st, ") "); i >= 0 {
			uptime = "exited " + st[i+2:]
		} else {
			uptime = "exited"
		}
	default:
		uptime = strings.ToLower(st)
	}
	r := strings.NewReplacer("About an hour", "1 hour", "About a minute", "1 minute", "Less than a second", "0 seconds",
		" minutes", " min", " minute", " min", " seconds", " s", " second", " s")
	return r.Replace(uptime), health
}

func (s *Server) containersView() containersData {
	s.mu.RLock()
	list, errText := s.containers, s.dockerErr
	s.mu.RUnlock()
	d := containersData{Error: errText, Total: len(list)}
	var top, sum uint64
	projects := map[string]bool{}
	for _, c := range list {
		top = max(top, c.MemBytes)
		sum += c.MemBytes
	}
	for _, c := range list {
		if c.Running() {
			d.Running++
		}
		v := containerView{ID: c.ID, Name: c.Name, Project: c.Project, Status: c.Status, State: c.State,
			Mem: fmtBytes(c.MemBytes), MemBytes: c.MemBytes, MemPct: "0", Running: c.Running()}
		if v.Project == "" {
			v.Project = "–"
		} else {
			projects[v.Project] = true
		}
		v.Uptime, v.Health = containerUptime(c.Status)
		if top > 0 && c.MemBytes > 0 {
			v.MemPct = fmt.Sprintf("%.1f", 100*float64(c.MemBytes)/float64(top))
		}
		if c.CPUOK {
			v.CPU, v.CPUVal = fmtCPU(c.CPUPercent), c.CPUPercent
		}
		d.Rows = append(d.Rows, v)
	}
	d.Projects = len(projects)
	d.MemTotal = fmtBytes(sum)
	return d
}

// fmtCPU: "0.4%", "12%", "150%" (100% = one core).
func fmtCPU(p float64) string {
	if p < 10 {
		return fmt.Sprintf("%.1f%%", p)
	}
	return fmt.Sprintf("%.0f%%", p)
}

func (s *Server) headerView() headerData {
	s.mu.RLock()
	sys := s.lastSys
	s.mu.RUnlock()
	now := time.Now()
	return headerData{Host: s.hostname, Uptime: fmtUptime(sys.Uptime), Since: "since " + now.Add(-sys.Uptime).Format("2 Jan"),
		Time: now.Format("15:04"), TimeISO: now.Format(time.RFC3339)}
}

// ---- Handlers ----

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionToken(r); !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(s.cfg.BaseURL()+"/"), http.StatusFound)
		return
	}
	// Everything on this page comes from caches the background loops keep
	// fresh (containers every 30 s, uptime every 5 min, backups every 10 s):
	// rendering never waits on Docker or VictoriaMetrics.
	s.mu.RLock()
	sys := s.lastSys
	s.mu.RUnlock()
	stats := buildStats(sys)
	s.mu.RLock()
	hist := make([]string, len(s.cpuHist))
	for i, v := range s.cpuHist {
		hist[i] = fmt.Sprintf("%.1f", v)
	}
	s.mu.RUnlock()
	stats[0].Hist = strings.Join(hist, ",") // stats[0] is the CPU cell
	s.render(w, http.StatusOK, "dashboard.html", dashboardPage{
		V: s.version, User: s.cfg.User, Page: "dashboard", Header: s.headerView(), Status: s.statusView(),
		Stats: stats, Apps: s.appsView(), Memory: s.memoryView(), Containers: s.containersView(),
		Notify: s.notifView(), Backup: s.backupView(),
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
	s.collectContainers(ctx)
	s.publishContainers()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"restarted": name})
}

// ---- Background work ----

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
	s.publishStatus() // the sentence mentions memory, disk, temperature, battery
}

func (s *Server) publishApps() {
	s.hub.Publish("apps", strings.TrimSpace(s.fragment("apps", s.appsView())))
	s.publishStatus()
}

func (s *Server) publishContainers() {
	s.hub.Publish("containers", strings.TrimSpace(s.fragment("containers", s.containersView())))
	s.publishStatus()
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

	// Containers: every 30s always (they feed /metrics, which
	// VictoriaMetrics scrapes whether or not anyone looks), and every 10s
	// while a dashboard is open. The ticker runs at the fast rate and
	// skips rounds when nobody watches and the data is still young.
	go func() { s.collectContainers(ctx); s.publishContainers() }()
	every(10*time.Second, func() {
		s.mu.RLock()
		age := time.Since(s.containersAt)
		s.mu.RUnlock()
		if s.hub.Count() == 0 && age < 29*time.Second {
			return
		}
		s.collectContainers(ctx)
		s.publishContainers()
	})

	// Memory breakdown: scan the host's processes every 10 s while a
	// dashboard is open, otherwise every 60 s (it also feeds /metrics). The
	// first scan waits for the first container list, so container
	// processes get their names.
	if s.memScan != nil {
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			s.collectMemory()
			s.publishMemory()
		}()
		every(memFastInterval, func() {
			s.mu.RLock()
			age := time.Since(s.mem.At)
			s.mu.RUnlock()
			if s.hub.Count() == 0 && age < memSlowInterval-time.Second {
				return
			}
			s.collectMemory()
			s.publishMemory()
		})
	}

	// Firing alerts, for the status sentence.
	go s.refreshAlerts(ctx)
	every(alertsInterval, func() { s.refreshAlerts(ctx) })

	// Uptime bars: ask VictoriaMetrics every 5 minutes (daily numbers do
	// not move faster than that). The first fetch waits a few seconds so
	// a Portal restart does not race VictoriaMetrics' own start.
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		s.refreshUptime(ctx)
	}()
	every(uptimeInterval, func() { s.refreshUptime(ctx) })

	// Backups panel: re-read status.json every 10s. The hub only sends
	// the fragment when the rendered HTML changed.
	s.publishBackup()
	every(backupPollInterval, s.publishBackup)

	// Notifications: poll ntfy every 15s, always (cheap: each poll asks
	// only for messages newer than the last one seen). The first poll
	// runs right away so the panel is filled before anyone looks.
	go s.refreshNotify(ctx)
	every(15*time.Second, func() { s.refreshNotify(ctx) })
	// Relative times ("5 min ago") in the pushed fragment would go stale
	// for new visitors; re-render once a minute (the hub drops it if the
	// HTML did not change).
	every(time.Minute, s.publishNotify)

	// Housekeeping: drop expired sessions and stale rate-limit entries.
	every(time.Hour, func() {
		if n, err := s.sessions.PurgeExpired(); err == nil && n > 0 {
			log.Printf("purged %d expired sessions", n)
		}
		s.limiter.Cleanup()
		s.notifyLimit.Cleanup()
	})
}
