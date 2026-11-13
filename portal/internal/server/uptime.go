package server

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"portal/internal/metricsdb"
)

// ---- 30-day uptime bars on the app tiles ----
//
// Where the numbers come from: every 30 s Portal health-checks each app and
// exposes the result as portal_app_up{app="focal"} 1 or 0 on /metrics.
// VictoriaMetrics scrapes that every 30 s and keeps 30 days of it. So for
// any day, "uptime" is simply the AVERAGE of that 0/1 series over the day:
// 2880 samples, 2877 of them 1 -> 99.9%.
//
// PromQL says exactly that: avg_over_time(portal_app_up[1d]) evaluated at
// midnight is the mean of the samples in the 24 h before it. Asking
// query_range for that expression with step=1d, starting at a local
// midnight, returns one value per calendar day (IST, because TZ is set).
// Today is not finished yet, so it is a separate instant query over
// [midnight, now].
//
// Overall % = all "up" samples / all samples over the days that have
// data, i.e. the daily averages weighted by how many samples each day
// has (count_over_time). Weighting matters for partial days: a day with
// one failed check out of 3 samples must not count as much as a full day.

const (
	uptimeDays     = 30
	uptimeInterval = 5 * time.Minute
	uptimeTimeout  = 10 * time.Second
)

// uptimeDay is one bar segment.
type uptimeDay struct {
	Start   time.Time // local midnight
	Ratio   float64   // 0..1, valid when Samples > 0
	Samples float64   // number of scraped samples that day (0 = no data)
}

// appUptime is one app's 30 days, oldest first.
type appUptime struct {
	Days [uptimeDays]uptimeDay
}

// uptimeData is the cached result of one refresh.
type uptimeData struct {
	At    time.Time
	Days  [uptimeDays]time.Time // local midnights, oldest first; Days[29] = today
	Apps  map[string]*appUptime
	Error string // "" or why the last refresh failed (old data is kept)
}

// dayStarts returns the local midnights of the last n days, oldest first;
// the last one is today's. time.Date normalises "day 0" and negative
// days, and handles DST if the zone has it (IST does not).
func dayStarts(now time.Time, n int) []time.Time {
	y, m, d := now.Date()
	out := make([]time.Time, n)
	for i := range n {
		out[i] = time.Date(y, m, d-(n-1-i), 0, 0, 0, 0, now.Location())
	}
	return out
}

// Queries. "by (app)" folds away labels such as job/instance that the
// scraper adds, so there is exactly one series per app.
const (
	qUptimeAvg   = `avg by (app) (avg_over_time(portal_app_up[%s]))`
	qUptimeCount = `sum by (app) (count_over_time(portal_app_up[%s]))`
)

// fetchUptime runs the four queries and assembles the per-app days.
func fetchUptime(ctx context.Context, vm *metricsdb.Client, now time.Time) (uptimeData, error) {
	starts := dayStarts(now, uptimeDays)
	d := uptimeData{At: now, Apps: map[string]*appUptime{}}
	copy(d.Days[:], starts)

	get := func(id string) *appUptime {
		a := d.Apps[id]
		if a == nil {
			a = &appUptime{}
			for i := range a.Days {
				a.Days[i].Start = starts[i]
			}
			d.Apps[id] = a
		}
		return a
	}

	// Complete days: evaluate at midnights starts[1]..starts[29]; the value
	// at starts[k] describes day k-1 (the 24 h that END at that midnight).
	first, last := starts[1], starts[uptimeDays-1]
	avg, err := vm.QueryRange(ctx, fmt.Sprintf(qUptimeAvg, "1d"), first, last, 24*time.Hour)
	if err != nil {
		return d, err
	}
	cnt, err := vm.QueryRange(ctx, fmt.Sprintf(qUptimeCount, "1d"), first, last, 24*time.Hour)
	if err != nil {
		return d, err
	}
	// Today so far: [midnight, now]. At least a minute so the window is
	// never empty right after midnight.
	window := max(time.Minute, now.Sub(starts[uptimeDays-1])).Truncate(time.Second)
	win := fmt.Sprintf("%ds", int(window.Seconds()))
	todayAvg, err := vm.Query(ctx, fmt.Sprintf(qUptimeAvg, win), now)
	if err != nil {
		return d, err
	}
	todayCnt, err := vm.Query(ctx, fmt.Sprintf(qUptimeCount, win), now)
	if err != nil {
		return d, err
	}

	// dayIndex finds which day a range-query timestamp closes. Matching
	// within an hour (instead of exact equality) tolerates a server that
	// rounds timestamps.
	dayIndex := func(t time.Time) int {
		for k := 1; k < uptimeDays; k++ {
			if diff := t.Sub(starts[k]); diff > -time.Hour && diff < time.Hour {
				return k - 1
			}
		}
		return -1
	}
	apply := func(series []metricsdb.Series, today bool, set func(*uptimeDay, float64)) {
		for _, s := range series {
			id := s.Labels["app"]
			if id == "" {
				continue
			}
			a := get(id)
			for _, p := range s.Points {
				if math.IsNaN(p.V) || math.IsInf(p.V, 0) {
					continue
				}
				k := uptimeDays - 1
				if !today {
					k = dayIndex(p.T)
				}
				if k >= 0 {
					set(&a.Days[k], p.V)
				}
			}
		}
	}
	setRatio := func(day *uptimeDay, v float64) { day.Ratio = min(1, max(0, v)) }
	setCount := func(day *uptimeDay, v float64) { day.Samples = v }
	apply(avg, false, setRatio)
	apply(cnt, false, setCount)
	apply(todayAvg, true, setRatio)
	apply(todayCnt, true, setCount)
	return d, nil
}

// ---- view ----

// uptimeSeg is one segment of the bar.
type uptimeSeg struct {
	Level string // good, warn, bad, none: a CSS class, no inline styles (CSP)
	Title string // tooltip: "Thu 12 Nov: 99.95%"
}

// uptimeView is what an app tile shows.
type uptimeView struct {
	Segs  []uptimeSeg
	Label string // "99.8% · 30d", "100% · since today", "No history yet"
	Pct   string // just the percentage, or the note when there is no data
	Span  string // what the history covers: "the last 30 days", "today only"
	Aria  string // the whole bar in words, for screen readers
	Level string // overall level, colours the label
}

func uptimeLevel(ratio float64) string {
	switch p := ratio * 100; {
	case p >= 99.9:
		return "good"
	case p >= 95:
		return "warn"
	}
	return "bad"
}

// fmtUptimePct rounds DOWN, so 99.96% shows as "99.9%" (never as a
// flattering "100%") and only a perfect record says 100%.
func fmtUptimePct(ratio float64, decimals int) string {
	if ratio >= 1 {
		return "100%"
	}
	scale := math.Pow(10, float64(decimals))
	return fmt.Sprintf("%.*f%%", decimals, math.Floor(ratio*100*scale)/scale)
}

// buildUptimeView turns one app's days into the bar. a may be nil (no
// data at all for this app). note is shown when there is no data, e.g.
// "History unavailable" while VictoriaMetrics is down.
func buildUptimeView(a *appUptime, days [uptimeDays]time.Time, note string) uptimeView {
	v := uptimeView{Segs: make([]uptimeSeg, uptimeDays)}
	var up, total, ratioSum float64
	firstData, withData, below := -1, 0, 0
	for i := range uptimeDays {
		label := days[i].Format("Mon 2 Jan")
		if i == uptimeDays-1 {
			label += " (today)"
		}
		seg := uptimeSeg{Level: "none", Title: label + ": no data"}
		if a != nil && a.Days[i].Samples > 0 {
			day := a.Days[i]
			seg.Level, seg.Title = uptimeLevel(day.Ratio), label+": "+fmtUptimePct(day.Ratio, 2)
			up += day.Ratio * day.Samples
			total += day.Samples
			ratioSum += day.Ratio
			withData++
			if firstData < 0 {
				firstData = i
			}
			if seg.Level != "good" {
				below++
			}
		}
		v.Segs[i] = seg
	}
	if withData == 0 {
		if note == "" {
			note = "No history yet"
		}
		v.Label, v.Pct, v.Level = note, note, "none"
		v.Aria = "Uptime history: " + strings.ToLower(note[:1]) + note[1:] + "."
		return v
	}
	overall := up / total
	if total == 0 { // cannot happen with Samples > 0, kept for safety
		overall = ratioSum / float64(withData)
	}
	span := uptimeDays - firstData // days from the first day with data to today
	var since string
	switch {
	case span == uptimeDays:
		since = fmt.Sprintf("%dd", uptimeDays)
	case span == 1:
		since = "since today"
	default:
		since = fmt.Sprintf("%dd", span)
	}
	v.Label = fmtUptimePct(overall, 1) + " · " + since
	v.Pct = fmtUptimePct(overall, 1)
	switch {
	case span == 1:
		v.Span = "today only"
	default:
		v.Span = fmt.Sprintf("the last %d days", span)
	}
	v.Level = uptimeLevel(overall)
	v.Aria = fmt.Sprintf("Uptime %s over the last %s: %d of %d days with data at or above 99.9%%.",
		fmtUptimePct(overall, 1), strings.TrimPrefix(since, "since "), withData-below, withData)
	if span == 1 {
		v.Aria = fmt.Sprintf("Uptime %s since today; less than a day of history.", fmtUptimePct(overall, 1))
	}
	return v
}

// uptimeFor returns the tile view for an app id from the cache.
func (s *Server) uptimeFor(id string) uptimeView {
	s.mu.RLock()
	d := s.uptime
	s.mu.RUnlock()
	note := ""
	switch {
	case !s.vm.Configured():
		note = "History off"
	case d.At.IsZero() && d.Error == "":
		note = "Loading history"
	case d.Error != "" && len(d.Apps) == 0:
		note = "History unavailable"
	}
	if d.At.IsZero() { // never fetched: still show 30 empty days ending today
		d.Days = [uptimeDays]time.Time(dayStarts(time.Now(), uptimeDays))
	}
	return buildUptimeView(d.Apps[id], d.Days, note)
}

// refreshUptime queries VictoriaMetrics and replaces the cache. On
// failure the previous data stays (with Error set) so a VM restart does
// not blank every tile.
func (s *Server) refreshUptime(ctx context.Context) {
	if !s.vm.Configured() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, uptimeTimeout)
	defer cancel()
	d, err := fetchUptime(ctx, s.vm, time.Now())
	s.setVMStatus(err)
	s.mu.Lock()
	if err != nil {
		if s.uptime.Error == "" {
			log.Printf("uptime: %v", err) // once per outage
		}
		s.uptime.Error = "VictoriaMetrics not reachable"
		if s.uptime.At.IsZero() {
			s.uptime.Days = d.Days
		}
	} else {
		s.uptime = d
	}
	s.mu.Unlock()
	s.publishApps()
}

// setVMStatus records whether the last VictoriaMetrics call worked. The
// /graphs page reads it to show its banner without waiting on VM.
func (s *Server) setVMStatus(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vmCheckedAt = time.Now()
	if err != nil {
		s.vmErr = err.Error()
	} else {
		s.vmErr = ""
	}
}
