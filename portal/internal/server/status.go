package server

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ---- The one sentence at the top of the dashboard ----
//
// It answers "is everything fine?" from state Portal already holds: app
// health checks, the backup status file, alerts firing in vmalert (read
// from VictoriaMetrics), Docker reachability and a few host readings.
// When all is well it says so in plain ink; each problem is one clause,
// marked amber or red AND worded so colour is never the only signal.

const alertsInterval = 30 * time.Second

// firingAlert is one series of ALERTS{alertstate="firing"}.
type firingAlert struct {
	Name, Severity, Subject string
}

type alertsData struct {
	At     time.Time
	List   []firingAlert
	Failed bool // the last query failed: "alerts unknown"
}

// refreshAlerts asks VictoriaMetrics which alerts vmalert reports as
// firing (vmalert writes its ALERTS series there).
func (s *Server) refreshAlerts(ctx context.Context) {
	if !s.vm.Configured() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	res, err := s.vm.Query(ctx, `ALERTS{alertstate="firing"}`, time.Now())
	s.mu.Lock()
	if err != nil {
		s.alerts.Failed = true
	} else {
		d := alertsData{At: time.Now()}
		for _, r := range res {
			subj := r.Labels["name"]
			if subj == "" {
				subj = r.Labels["app"]
			}
			d.List = append(d.List, firingAlert{Name: r.Labels["alertname"], Severity: r.Labels["severity"], Subject: subj})
		}
		sort.Slice(d.List, func(i, j int) bool {
			if d.List[i].Name != d.List[j].Name {
				return d.List[i].Name < d.List[j].Name
			}
			return d.List[i].Subject < d.List[j].Subject
		})
		s.alerts = d
	}
	s.mu.Unlock()
	s.publishStatus()
}

// statusPart is one clause; Level is "", "warn" or "bad".
type statusPart struct {
	Text, Level string
}

type statusView struct {
	Parts []statusPart
	Level string // the worst part
	Plain string // the sentence as text (title, tests)
}

var levelRank = map[string]int{"": 0, "warn": 1, "bad": 2}

func worse(a, b string) string {
	if levelRank[b] > levelRank[a] {
		return b
	}
	return a
}

// joinNames: "A", "A and B", "A, B and C".
func joinNames(n []string) string {
	switch len(n) {
	case 0:
		return ""
	case 1:
		return n[0]
	}
	return strings.Join(n[:len(n)-1], ", ") + " and " + n[len(n)-1]
}

// statusInput is everything the sentence depends on, so it can be tested
// without a server.
type statusInput struct {
	Apps       appsData
	Backup     backupData
	Alerts     alertsData
	AlertsOn   bool            // VictoriaMetrics configured
	Silenced   map[string]bool // alert subjects Portal silenced (stopped on purpose)
	DockerErr  string
	MemPct     float64 // used, 0..100
	DiskPct    float64
	TempC      float64
	TempOK     bool
	OnBattery  bool
	BatteryPct int
}

func buildStatus(in statusInput) statusView {
	var v statusView
	add := func(text, level string) {
		v.Parts = append(v.Parts, statusPart{text, level})
		v.Level = worse(v.Level, level)
	}

	// Apps.
	var down, stopped []string
	for _, a := range in.Apps.Tiles {
		switch a.State {
		case "down":
			down = append(down, a.Name)
		case "stopped":
			stopped = append(stopped, a.Name)
		}
	}
	switch {
	case in.Apps.Total == 0:
		add("no apps configured", "")
	case len(down) == 1:
		add(down[0]+" is down", "bad")
	case len(down) > 1:
		add(joinNames(down)+" are down", "bad")
	case in.Apps.Up == in.Apps.Total:
		add(fmt.Sprintf("all %d apps up", in.Apps.Total), "")
	case len(stopped) > 0 && in.Apps.Up+len(stopped) == in.Apps.Total:
		add(fmt.Sprintf("all %d running apps up", in.Apps.Up), "")
	default:
		add(fmt.Sprintf("%d of %d apps checked", in.Apps.Up, in.Apps.Total), "")
	}

	if len(stopped) > 0 {
		add(joinNames(stopped)+" stopped", "")
	}

	// Backups.
	add(in.Backup.Summary, in.Backup.SummaryLevel)

	// Alerts. Those about something stopped on purpose from Portal (it
	// holds a silence for it) are counted apart, in plain ink.
	var live []firingAlert
	quiet := 0
	for _, a := range in.Alerts.List {
		if in.Silenced[a.Subject] {
			quiet++
			continue
		}
		live = append(live, a)
	}
	in.Alerts.List = live
	switch {
	case !in.AlertsOn:
	case in.Alerts.Failed && in.Alerts.At.IsZero():
		add("alerts unknown (VictoriaMetrics not reachable)", "warn")
	case len(in.Alerts.List) == 0 && !in.Alerts.At.IsZero():
		add("no alerts firing", "")
	case len(in.Alerts.List) > 0:
		level := "warn"
		var names []string
		seen := map[string]int{}
		for _, a := range in.Alerts.List {
			if a.Severity == "urgent" || a.Severity == "critical" {
				level = "bad"
			}
			if seen[a.Name] == 0 {
				names = append(names, a.Name)
			}
			seen[a.Name]++
		}
		for i, n := range names {
			if seen[n] > 1 {
				names[i] = fmt.Sprintf("%s ×%d", n, seen[n])
			}
		}
		add(plural(len(in.Alerts.List), "alert", "alerts")+" firing: "+strings.Join(names, ", "), level)
	}
	if quiet > 0 {
		add(plural(quiet, "alert", "alerts")+" silenced (stopped on purpose)", "")
	}

	// Host: only mentioned when something needs attention.
	if in.DockerErr != "" {
		add("Docker not reachable", "bad")
	}
	if in.MemPct >= 90 {
		add(fmt.Sprintf("memory %.0f%% used", in.MemPct), level(in.MemPct, 90, 97))
	}
	if in.DiskPct >= 80 {
		add(fmt.Sprintf("disk %.0f%% full", in.DiskPct), level(in.DiskPct, 80, 92))
	}
	if in.TempOK && in.TempC >= 75 {
		add(fmt.Sprintf("CPU at %.0f °C", in.TempC), level(in.TempC, 75, 90))
	}
	if in.OnBattery {
		lv := "warn"
		if in.BatteryPct < 20 {
			lv = "bad"
		}
		add(fmt.Sprintf("on battery, %d%%", in.BatteryPct), lv)
	}

	// Problems first, so the sentence leads with what needs attention.
	sort.SliceStable(v.Parts, func(i, j int) bool {
		return levelRank[v.Parts[i].Level] > levelRank[v.Parts[j].Level]
	})
	var texts []string
	for i := range v.Parts {
		if i == 0 && v.Parts[i].Text != "" {
			v.Parts[i].Text = strings.ToUpper(v.Parts[i].Text[:1]) + v.Parts[i].Text[1:]
		}
		texts = append(texts, v.Parts[i].Text)
	}
	v.Plain = strings.Join(texts, " · ")
	// On a phone the sentence wraps; keep "2 h ago" and "01:20" in one
	// piece by tying numbers to what follows them with no-break spaces.
	for i := range v.Parts {
		t := nbspAfterNumber.ReplaceAllString(v.Parts[i].Text, "$1\u00a0$2")
		v.Parts[i].Text = strings.ReplaceAll(t, " ago", "\u00a0ago")
	}
	return v
}

var nbspAfterNumber = regexp.MustCompile(`(\d) (\S)`)

func (s *Server) statusView() statusView {
	s.mu.RLock()
	sys, al, dockerErr := s.lastSys, s.alerts, s.dockerErr
	s.mu.RUnlock()
	return buildStatus(statusInput{
		Apps: s.appsView(), Backup: s.backupView(), Alerts: al, AlertsOn: s.vm.Configured(),
		DockerErr: dockerErr, MemPct: pct(sys.MemUsed, sys.MemTotal), DiskPct: pct(sys.DiskUsed, sys.DiskTotal),
		Silenced: s.silencedSubjects(),
		TempC:    sys.TempC, TempOK: sys.TempOK, OnBattery: sys.BatteryOK && !sys.ACOnline, BatteryPct: sys.BatteryPercent,
	})
}

func (s *Server) publishStatus() {
	s.hub.Publish("status", strings.TrimSpace(s.fragment("status", s.statusView())))
}
