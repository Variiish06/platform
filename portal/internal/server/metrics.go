package server

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// Metrics counts events for the /metrics endpoint.
//
// The Prometheus text format is simple enough to write by hand, which
// avoids pulling in the official client library (and its memory):
//
//	# TYPE name counter
//	name{label="value"} 42
type Metrics struct {
	mu       sync.Mutex
	requests map[string]uint64 // "handler|code"
	logins   map[string]uint64 // ok, failed, rate_limited
	verifies map[string]uint64 // allowed, denied, unknown_host
}

// NewMetrics returns zeroed counters.
func NewMetrics() *Metrics {
	return &Metrics{requests: map[string]uint64{}, logins: map[string]uint64{}, verifies: map[string]uint64{}}
}

func (m *Metrics) Request(handler string, code int) {
	m.mu.Lock()
	m.requests[fmt.Sprintf("%s|%d", handler, code)]++
	m.mu.Unlock()
}

func (m *Metrics) Login(result string)  { m.mu.Lock(); m.logins[result]++; m.mu.Unlock() }
func (m *Metrics) Verify(result string) { m.mu.Lock(); m.verifies[result]++; m.mu.Unlock() }

func sortedKeys(m map[string]uint64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// handleMetrics serves Prometheus metrics.
//
// Access: a signed-in browser, or a scraper on this machine talking to
// 127.0.0.1:7080 directly. The two are told apart by X-Forwarded-For,
// which Caddy adds to everything it proxies and a direct local request
// does not have.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Forwarded-For") != "" {
		if _, ok := s.sessionToken(r); !ok {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
	}
	var b strings.Builder
	gauge := func(name, help string, v float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
	}
	bool01 := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}

	m := s.metrics
	m.mu.Lock()
	b.WriteString("# HELP portal_http_requests_total Requests handled, by handler and status code.\n# TYPE portal_http_requests_total counter\n")
	for _, k := range sortedKeys(m.requests) {
		h, code, _ := strings.Cut(k, "|")
		fmt.Fprintf(&b, "portal_http_requests_total{handler=%q,code=%q} %d\n", h, code, m.requests[k])
	}
	b.WriteString("# HELP portal_login_attempts_total Login attempts by result.\n# TYPE portal_login_attempts_total counter\n")
	for _, k := range []string{"ok", "failed", "rate_limited"} {
		fmt.Fprintf(&b, "portal_login_attempts_total{result=%q} %d\n", k, m.logins[k])
	}
	b.WriteString("# HELP portal_forward_auth_total Forward-auth decisions by result.\n# TYPE portal_forward_auth_total counter\n")
	for _, k := range []string{"allowed", "denied", "unknown_host"} {
		fmt.Fprintf(&b, "portal_forward_auth_total{result=%q} %d\n", k, m.verifies[k])
	}
	m.mu.Unlock()

	gauge("portal_sessions_active", "Live login sessions.", float64(s.sessions.CountActive()))
	gauge("portal_sse_clients", "Open dashboard event streams.", float64(s.hub.Count()))

	b.WriteString("# HELP portal_app_up Whether the app's last health check succeeded.\n# TYPE portal_app_up gauge\n")
	list := s.apps.List()
	for _, a := range list {
		st, _ := s.apps.Status(a.ID)
		fmt.Fprintf(&b, "portal_app_up{app=%q} %g\n", a.ID, bool01(st.Up))
	}
	b.WriteString("# HELP portal_app_response_seconds Response time of the app's last health check.\n# TYPE portal_app_response_seconds gauge\n")
	for _, a := range list {
		st, _ := s.apps.Status(a.ID)
		fmt.Fprintf(&b, "portal_app_response_seconds{app=%q} %g\n", a.ID, st.Latency.Seconds())
	}

	s.mu.RLock()
	sys := s.lastSys
	s.mu.RUnlock()
	gauge("portal_host_cpu_percent", "Host CPU busy percentage.", sys.CPUPercent)
	gauge("portal_host_load1", "Host 1-minute load average.", sys.Load1)
	gauge("portal_host_memory_used_bytes", "Host memory in use (total minus available).", float64(sys.MemUsed))
	gauge("portal_host_memory_total_bytes", "Host memory installed.", float64(sys.MemTotal))
	gauge("portal_host_disk_used_bytes", "Used bytes on the data filesystem.", float64(sys.DiskUsed))
	gauge("portal_host_disk_total_bytes", "Size of the data filesystem.", float64(sys.DiskTotal))
	gauge("portal_host_uptime_seconds", "Host uptime.", sys.Uptime.Seconds())
	if sys.TempOK {
		gauge("portal_host_cpu_temperature_celsius", "CPU temperature.", sys.TempC)
	}
	if sys.BatteryOK {
		gauge("portal_host_battery_percent", "Battery charge.", float64(sys.BatteryPercent))
		gauge("portal_host_ac_online", "1 when the charger is connected.", bool01(sys.ACOnline))
	}
	if sys.WiFiOK {
		gauge("portal_host_wifi_signal_dbm", "WiFi signal level.", float64(sys.WiFiDBm))
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	gauge("portal_go_goroutines", "Goroutines in the Portal process.", float64(runtime.NumGoroutine()))
	gauge("portal_go_heap_alloc_bytes", "Heap bytes in use by Portal.", float64(ms.HeapAlloc))
	gauge("portal_go_sys_bytes", "Bytes Portal obtained from the OS.", float64(ms.Sys))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(b.String()))
}
