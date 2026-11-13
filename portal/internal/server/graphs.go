package server

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"portal/internal/metricsdb"
)

// ---- /graphs: charts drawn in the browser from VictoriaMetrics data ----
//
// The browser asks GET /api/graphs/cpu?range=24h. It never sends PromQL:
// the name picks one entry of the fixed allowlist below, and the range
// picks one of five fixed (duration, step) pairs. So a signed-in page
// cannot be turned into a general query console for the metrics database
// (and a bug in the page cannot run an expensive query).
//
// A range query returns one point per step. The step is chosen so every
// range draws ~360 points: enough detail for a chart a few hundred pixels
// wide, and the same payload size whether you look at 1 hour or 30 days.

// graphRange is one choice of the range selector.
type graphRange struct {
	Dur  time.Duration
	Step time.Duration
}

var graphRanges = map[string]graphRange{
	"1h":  {time.Hour, 10 * time.Second},          // 360 points
	"6h":  {6 * time.Hour, time.Minute},           // 360
	"24h": {24 * time.Hour, 4 * time.Minute},      // 360
	"7d":  {7 * 24 * time.Hour, 30 * time.Minute}, // 336
	"30d": {30 * 24 * time.Hour, 2 * time.Hour},   // 360
}

// graphRangeOrder is the order of the buttons on the page.
var graphRangeOrder = []string{"1h", "6h", "24h", "7d", "30d"}

// graphQuery is one line on a chart.
type graphQuery struct {
	Label string // legend text; ignored when LabelFrom is set
	// Expr is PromQL. "$w" is replaced by the rate window: max(5m, step).
	// rate() over a window shorter than the step would skip samples
	// between points (a 2-hour step with a 5-minute window looks at 5
	// minutes out of every 120), so the window grows with the step.
	Expr      string
	LabelFrom string // take one legend entry per series from this label
}

// graphDef is one chart.
type graphDef struct {
	Name, Title, Unit string // unit: percent, bytes, bytes_s, celsius, seconds, number
	Desc              string
	Queries           []graphQuery
	TopN              int // keep only the N series with the highest latest value
	// Stacked draws filled areas on top of each other (the series are
	// parts of one whole). Order fixes the series order by label value,
	// bottom first; Names gives each value its legend text.
	Stacked bool
	Order   []string
	Names   map[string]string
}

// graphDefs is the allowlist, in page order. Metric names: node_exporter
// (collectors cpu, loadavg, meminfo, filesystem, hwmon, netdev,
// powersupplyclass), Portal's own /metrics, and the backup container's
// textfile metrics.
var graphDefs = []graphDef{
	{Name: "cpu", Title: "CPU usage", Unit: "percent", Desc: "All cores, busy time",
		Queries: []graphQuery{{Label: "CPU", Expr: `100 - avg(rate(node_cpu_seconds_total{mode="idle"}[$w])) * 100`}}},
	{Name: "load", Title: "Load average", Unit: "number", Desc: "Runnable tasks (1, 5, 15 min)",
		Queries: []graphQuery{
			{Label: "1 min", Expr: `max(node_load1)`},
			{Label: "5 min", Expr: `max(node_load5)`},
			{Label: "15 min", Expr: `max(node_load15)`}}},
	{Name: "memory", Title: "RAM used", Unit: "percent", Desc: "Total minus available",
		Queries: []graphQuery{{Label: "RAM used", Expr: `100 * (1 - max(node_memory_MemAvailable_bytes) / max(node_memory_MemTotal_bytes))`}}},
	{Name: "memory_bytes", Title: "RAM", Unit: "bytes", Desc: "Used and installed",
		Queries: []graphQuery{
			{Label: "Used", Expr: `max(node_memory_MemTotal_bytes) - max(node_memory_MemAvailable_bytes)`},
			{Label: "Total", Expr: `max(node_memory_MemTotal_bytes)`}}},
	{Name: "memory_groups", Title: "Memory by group", Unit: "bytes", Desc: "What holds the RAM (Portal's host scan)", Stacked: true,
		Queries: []graphQuery{{Expr: `max by (group) (portal_memory_group_bytes{group!="unaccounted"})`, LabelFrom: "group"}},
		Order:   []string{"containers", "claude", "docker_engine", "user", "system", "kernel", "cache", "free"},
		Names: map[string]string{"containers": "Containers", "claude": "Claude Code", "docker_engine": "Docker engine",
			"user": "User processes", "system": "System services", "kernel": "Kernel", "cache": "Page cache", "free": "Free"}},
	{Name: "disk", Title: "Disk used", Unit: "percent", Desc: "Root filesystem /",
		Queries: []graphQuery{{Label: "/", Expr: `100 * (1 - max(node_filesystem_avail_bytes{mountpoint="/"}) / max(node_filesystem_size_bytes{mountpoint="/"}))`}}},
	{Name: "temperature", Title: "CPU temperature", Unit: "celsius", Desc: "coretemp package sensor",
		Queries: []graphQuery{{Label: "Package", Expr: `max(node_hwmon_temp_celsius * on(chip, sensor) group_left node_hwmon_sensor_label{label="Package id 0"}) or max(node_thermal_zone_temp{type="x86_pkg_temp"})`}}},
	{Name: "network", Title: "Network (WiFi)", Unit: "bytes_s", Desc: "wlp0s20f3",
		Queries: []graphQuery{
			{Label: "Received", Expr: `sum(rate(node_network_receive_bytes_total{device="wlp0s20f3"}[$w]))`},
			{Label: "Sent", Expr: `sum(rate(node_network_transmit_bytes_total{device="wlp0s20f3"}[$w]))`}}},
	{Name: "battery", Title: "Battery", Unit: "percent", Desc: "Charge, and charger (100 = connected)",
		Queries: []graphQuery{
			{Label: "Charge", Expr: `max(node_power_supply_capacity{power_supply="BAT0"})`},
			{Label: "Charger", Expr: `max(node_power_supply_online{power_supply="ADP1"}) * 100`}}},
	{Name: "containers_cpu", Title: "Container CPU", Unit: "percent", Desc: "Top 8 now; 100% = one core", TopN: 8,
		Queries: []graphQuery{{Expr: `max by (name) (portal_container_cpu_percent)`, LabelFrom: "name"}}},
	{Name: "containers_memory", Title: "Container memory", Unit: "bytes", Desc: "Top 8 now", TopN: 8,
		Queries: []graphQuery{{Expr: `max by (name) (portal_container_memory_bytes)`, LabelFrom: "name"}}},
	{Name: "app_response", Title: "App response time", Unit: "seconds", Desc: "Portal's health checks",
		Queries: []graphQuery{{Expr: `max by (app) (portal_app_response_seconds)`, LabelFrom: "app"}}},
	{Name: "backup_duration", Title: "Backup duration", Unit: "seconds", Desc: "Last run",
		Queries: []graphQuery{{Label: "Duration", Expr: `max(backup_last_duration_seconds)`}}},
	{Name: "backup_repo", Title: "Backup repository", Unit: "bytes", Desc: "Size of the restic repo",
		Queries: []graphQuery{{Label: "Repo size", Expr: `max(backup_repo_size_bytes)`}}},
}

var graphByName = func() map[string]*graphDef {
	m := make(map[string]*graphDef, len(graphDefs))
	for i := range graphDefs {
		m[graphDefs[i].Name] = &graphDefs[i]
	}
	return m
}()

const (
	graphCacheTTL      = 20 * time.Second
	graphErrorTTL      = 5 * time.Second
	graphQueryTimeout  = 8 * time.Second
	graphMaxSeries     = 12
	graphMaxBodyBytes  = 512 << 10
	graphRateWindowMin = 5 * time.Minute
)

// graphCache remembers recent answers per name+range. Every open /graphs
// tab refreshes 13 charts every 30-60 s; with the cache, several tabs (or
// a quick range toggle) cost VictoriaMetrics nothing extra. It also folds
// concurrent requests for the same key into one query ("single flight").
type graphCache struct {
	mu      sync.Mutex
	entries map[string]*graphEntry
}

type graphEntry struct {
	done    chan struct{} // closed when the fetch finished
	expires time.Time
	body    []byte
	status  int
}

func newGraphCache() *graphCache { return &graphCache{entries: map[string]*graphEntry{}} }

// get returns the cached answer for key, or runs fetch (once, even if many
// callers arrive together) and caches its result.
func (c *graphCache) get(key string, now time.Time, fetch func() (int, []byte)) (int, []byte) {
	c.mu.Lock()
	e := c.entries[key]
	if e != nil {
		select {
		case <-e.done:
			if now.Before(e.expires) {
				c.mu.Unlock()
				return e.status, e.body
			}
			e = nil // expired: fetch again below
		default: // in flight: wait for it
			c.mu.Unlock()
			<-e.done
			return e.status, e.body
		}
	}
	// Drop expired entries so the map cannot grow (it is bounded by
	// 13 names x 5 ranges anyway).
	for k, old := range c.entries {
		select {
		case <-old.done:
			if !now.Before(old.expires) {
				delete(c.entries, k)
			}
		default:
		}
	}
	e = &graphEntry{done: make(chan struct{})}
	c.entries[key] = e
	c.mu.Unlock()

	e.status, e.body = fetch()
	ttl := graphCacheTTL
	if e.status != http.StatusOK {
		ttl = graphErrorTTL
	}
	e.expires = time.Now().Add(ttl)
	close(e.done)
	return e.status, e.body
}

// graphSeries is one legend entry in the JSON answer.
type graphSeries struct {
	Label string `json:"label"`
}

// graphResult is the JSON answer, shaped for uPlot: data[0] is the shared
// x axis (unix seconds), data[i] the values of series[i-1], null = gap.
type graphResult struct {
	Name    string            `json:"name"`
	Title   string            `json:"title"`
	Unit    string            `json:"unit"`
	Range   string            `json:"range"`
	Stacked bool              `json:"stacked,omitempty"`
	Step    int64             `json:"step"`
	Series  []graphSeries     `json:"series"`
	Data    []json.RawMessage `json:"data"`
}

// alignedSeries is one line on the shared time grid.
type alignedSeries struct {
	label string
	vals  []float64 // NaN = no value
	last  float64   // latest real value, for TopN ranking
}

// rateWindow returns the window substituted for $w.
func rateWindow(step time.Duration) string {
	w := max(graphRateWindowMin, step)
	return strconv.FormatInt(int64(w.Seconds()), 10) + "s"
}

// alignSeries puts VM points on the grid start, start+step, ... (n slots).
// VM evaluates exactly at those instants, but rounding to the nearest slot
// keeps this correct even if a server shifts them slightly.
func alignSeries(label string, pts []metricsdb.Point, start time.Time, step time.Duration, n int) alignedSeries {
	a := alignedSeries{label: label, vals: make([]float64, n), last: math.NaN()}
	for i := range a.vals {
		a.vals[i] = math.NaN()
	}
	for _, p := range pts {
		if math.IsNaN(p.V) || math.IsInf(p.V, 0) {
			continue
		}
		i := int(math.Round(float64(p.T.Sub(start)) / float64(step)))
		if i >= 0 && i < n {
			a.vals[i] = p.V
		}
	}
	for i := n - 1; i >= 0; i-- {
		if !math.IsNaN(a.vals[i]) {
			a.last = a.vals[i]
			break
		}
	}
	return a
}

// topN keeps the n series with the highest latest value (series that have
// no value at all sort last), largest first.
func topN(list []alignedSeries, n int) []alignedSeries {
	sort.SliceStable(list, func(i, j int) bool {
		li, lj := list[i].last, list[j].last
		switch {
		case math.IsNaN(lj):
			return !math.IsNaN(li)
		case math.IsNaN(li):
			return false
		}
		return li > lj
	})
	if n > 0 && len(list) > n {
		list = list[:n]
	}
	return list
}

// encodeValues writes [1.5,null,2] compactly: 4 significant digits are
// plenty for a chart and keep 30-day payloads small.
func encodeValues(vals []float64) json.RawMessage {
	var b bytes.Buffer
	b.Grow(len(vals) * 6)
	b.WriteByte('[')
	for i, v := range vals {
		if i > 0 {
			b.WriteByte(',')
		}
		if math.IsNaN(v) {
			b.WriteString("null")
			continue
		}
		b.WriteString(strconv.FormatFloat(v, 'g', 4, 64))
	}
	b.WriteByte(']')
	return b.Bytes()
}

// runGraph executes one allowlisted chart for one range.
func runGraph(ctx context.Context, vm *metricsdb.Client, def *graphDef, rangeName string, now time.Time) (graphResult, error) {
	rg := graphRanges[rangeName]
	// Align the grid to the step so successive refreshes ask for the same
	// instants (and points do not "swim" left by a few seconds each time).
	end := now.Truncate(rg.Step)
	start := end.Add(-rg.Dur)
	n := int(rg.Dur/rg.Step) + 1

	var lines []alignedSeries
	for _, q := range def.Queries {
		expr := strings.ReplaceAll(q.Expr, "$w", rateWindow(rg.Step))
		res, err := vm.QueryRange(ctx, expr, start, end, rg.Step)
		if err != nil {
			return graphResult{}, err
		}
		for _, s := range res {
			label := q.Label
			if q.LabelFrom != "" {
				label = s.Labels[q.LabelFrom]
				if label == "" {
					continue
				}
			}
			lines = append(lines, alignSeries(label, s.Points, start, rg.Step, n))
		}
	}
	if def.TopN > 0 {
		lines = topN(lines, def.TopN)
	}
	if len(def.Order) > 0 {
		rank := map[string]int{}
		for i, k := range def.Order {
			rank[k] = i + 1
		}
		kept := lines[:0]
		for _, l := range lines {
			if rank[l.label] > 0 {
				kept = append(kept, l)
			}
		}
		lines = kept
		sort.SliceStable(lines, func(i, j int) bool { return rank[lines[i].label] < rank[lines[j].label] })
	}
	for i := range lines {
		if n, ok := def.Names[lines[i].label]; ok {
			lines[i].label = n
		}
	}
	if len(lines) > graphMaxSeries {
		lines = lines[:graphMaxSeries]
	}

	out := graphResult{Name: def.Name, Title: def.Title, Unit: def.Unit, Range: rangeName, Stacked: def.Stacked,
		Step: int64(rg.Step.Seconds()), Series: []graphSeries{}}
	ts := make([]float64, n)
	for i := range ts {
		ts[i] = float64(start.Add(time.Duration(i) * rg.Step).Unix())
	}
	tsJSON, _ := json.Marshal(ts) // whole seconds: no precision lost
	out.Data = append(out.Data, tsJSON)
	for _, l := range lines {
		out.Series = append(out.Series, graphSeries{Label: l.label})
		out.Data = append(out.Data, encodeValues(l.vals))
	}
	return out, nil
}

func jsonError(code int, msg string) (int, []byte) {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": msg})
	return code, b
}

// handleGraphData serves GET /api/graphs/{name}?range=24h.
func (s *Server) handleGraphData(w http.ResponseWriter, r *http.Request) {
	reply := func(code int, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		w.Write(body)
	}
	if _, ok := s.sessionToken(r); !ok {
		reply(jsonError(http.StatusUnauthorized, "not signed in"))
		return
	}
	def, ok := graphByName[r.PathValue("name")]
	if !ok {
		reply(jsonError(http.StatusNotFound, "unknown graph"))
		return
	}
	rangeName := r.URL.Query().Get("range")
	if rangeName == "" {
		rangeName = "24h"
	}
	if _, ok := graphRanges[rangeName]; !ok {
		reply(jsonError(http.StatusBadRequest, "range must be one of 1h, 6h, 24h, 7d, 30d"))
		return
	}
	if !s.vm.Configured() {
		reply(jsonError(http.StatusServiceUnavailable, "PORTAL_VM_URL is not set"))
		return
	}
	reply(s.graphCache.get(def.Name+"|"+rangeName, time.Now(), func() (int, []byte) {
		// Detached from this request: other callers may be waiting on the
		// same fetch, so one tab closing must not cancel it for them.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), graphQueryTimeout)
		defer cancel()
		res, err := runGraph(ctx, s.vm, def, rangeName, time.Now())
		s.setVMStatus(err)
		if err != nil {
			return jsonError(http.StatusServiceUnavailable, "VictoriaMetrics not reachable or query failed")
		}
		body, err := json.Marshal(res)
		if err != nil || len(body) > graphMaxBodyBytes {
			return jsonError(http.StatusBadGateway, "graph data too large")
		}
		return http.StatusOK, body
	}))
}

// ---- the page ----

type graphPanel struct {
	Name, Title, Unit, Desc string
	Stacked                 bool
}

type graphsPage struct {
	V, User string
	Page    string
	Header  headerData
	Panels  []graphPanel
	Ranges  []string
	VMState string // "ok", "down", "off", "unknown"
}

func (s *Server) handleGraphsPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionToken(r); !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(s.cfg.BaseURL()+"/graphs"), http.StatusFound)
		return
	}
	p := graphsPage{V: s.version, Page: "graphs", Header: s.headerView(), User: s.cfg.User, Ranges: graphRangeOrder}
	for _, d := range graphDefs {
		p.Panels = append(p.Panels, graphPanel{Name: d.Name, Title: d.Title, Unit: d.Unit, Desc: d.Desc, Stacked: d.Stacked})
	}
	// The banner comes from the LAST known VM status (refreshed in the
	// background and by every chart fetch), so rendering this page never
	// waits on VictoriaMetrics. graphs.js updates the banner live.
	s.mu.RLock()
	checked, vmErr := s.vmCheckedAt, s.vmErr
	s.mu.RUnlock()
	switch {
	case !s.vm.Configured():
		p.VMState = "off"
	case checked.IsZero():
		p.VMState = "unknown"
	case vmErr != "":
		p.VMState = "down"
	default:
		p.VMState = "ok"
	}
	s.render(w, http.StatusOK, "graphs.html", p)
}
