package server

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"portal/internal/procmem"
)

// ---- Memory panel: "what is taking the RAM?" ----
//
// The scan (package procmem) reads the host's /proc, mounted read-only at
// PORTAL_HOST_PROC. It costs a few milliseconds, so it runs every 10 s
// while a dashboard is open and every 60 s otherwise (the metrics below
// are scraped every 30 s either way). Pages and /metrics only read the
// cached result.

const (
	memFastInterval = 10 * time.Second
	memSlowInterval = 60 * time.Second
	// memMetricsMaxAge: after this long without a scan, the group metrics
	// are left out rather than repeating stale numbers.
	memMetricsMaxAge = 3 * time.Minute
)

// memGroupInfo is the fixed wording for each group.
var memGroupInfo = map[string]struct{ Label, Note string }{
	procmem.GContainers:  {"Containers", "Processes inside each container, by compose project."},
	procmem.GClaude:      {"Claude Code sessions", "Each claude CLI with everything it started: MCP servers, node, shells."},
	procmem.GEngine:      {"Docker engine", "dockerd, containerd and one shim per running container."},
	procmem.GUser:        {"Other user processes", "Your processes outside the groups above: tmux, user services, tools."},
	procmem.GSystem:      {"System services", "Root and other system accounts outside containers."},
	procmem.GKernel:      {"Kernel", "The kernel's own allocations."},
	procmem.GCache:       {"Page cache", "Memory Linux lends to file caching and takes back the moment a program needs it. Not “used”."},
	procmem.GFree:        {"Free", "Not holding anything right now."},
	procmem.GUnaccounted: {"Unaccounted", "MemTotal minus everything above: memory the kernel does not attribute to a process (graphics buffers, drivers), less pages that processes share and are counted twice."},
}

// collectMemory runs one scan and caches it.
func (s *Server) collectMemory() {
	if s.memScan == nil {
		return
	}
	s.mu.RLock()
	list := s.containers
	s.mu.RUnlock()
	names := make(map[string]procmem.Container, len(list))
	for _, c := range list {
		names[c.ID] = procmem.Container{Name: c.Name, Project: c.Project}
	}
	b, err := s.memScan.Scan(names, time.Now())
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if s.memErr == "" {
			log.Printf("memory scan: %v", err) // once per outage
		}
		s.memErr = "Could not read the host's /proc: " + err.Error()
		return
	}
	s.mem, s.memErr = b, ""
}

func (s *Server) memSnapshot() (procmem.Breakdown, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mem, s.memErr
}

func (s *Server) publishMemory() {
	s.hub.Publish("memory", strings.TrimSpace(s.fragment("memory", s.memoryView())))
}

// ---- view ----

type memSeg struct {
	Key, Label, Bytes string
	Pct               string // data-pct for the bar width
}

type memRow struct {
	Label, Sub, Bytes, Pct, Swap string
	PID                          int
}

type memProject struct {
	Name, Bytes, Pct string
	Rows             []memRow
}

type memSession struct {
	PID                       int
	Where, Name, Status, Args string
	Age, Started              string
	TS                        int64
	Bytes, Pct, Swap          string
	Procs                     int
}

type memGroup struct {
	Key, Label, Note string
	Bytes, Pct, Swap string
	Count            string // "6 sessions", "39 processes", ""
	Rows             []memRow
	Projects         []memProject
	Sessions         []memSession
	Negative         bool // unaccounted below zero: shown, not drawn
}

type memView struct {
	State   string // "ok", "off" (not configured), "error", "loading"
	Error   string
	Verdict string
	Aria    string // the bar in words
	Segs    []memSeg
	Groups  []memGroup
	Total   string
	Swap    string // "1.2 GiB of 4.0 GiB swap in use" or ""
	Took    string // "4 ms, 146 processes"
	At      string
}

// fmtMem formats bytes in binary units with one decimal: "334.2 MiB",
// "3.8 GiB". Negative values (unaccounted) keep their sign.
func fmtMem(b int64) string {
	sign := ""
	if b < 0 {
		sign, b = "−", -b
	}
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%s%.1f GiB", sign, float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%s%.1f MiB", sign, float64(b)/(1<<20))
	case b == 0:
		return "0"
	}
	return fmt.Sprintf("%s%.1f KiB", sign, float64(b)/(1<<10))
}

func fmtPct(part, total int64) string {
	if total <= 0 {
		return "–"
	}
	p := 100 * float64(part) / float64(total)
	if math.Abs(p) < 0.05 {
		return "0.0%" // not "-0.0%" for a tiny negative remainder
	}
	if math.Abs(p) < 10 {
		return fmt.Sprintf("%.1f%%", p)
	}
	return fmt.Sprintf("%.0f%%", p)
}

func pctAttr(part, total int64) string {
	if total <= 0 || part <= 0 {
		return "0"
	}
	return fmt.Sprintf("%.2f", 100*float64(part)/float64(total))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// memVerdict is the one plain sentence above the bar.
func memVerdict(b procmem.Breakdown) string {
	type cand struct {
		label string
		bytes int64
	}
	var cs []cand
	if g := b.Group(procmem.GClaude); g.Bytes > 0 {
		cs = append(cs, cand{fmt.Sprintf("Claude Code sessions %s (%d)", fmtMem(g.Bytes), g.Count), g.Bytes})
	}
	if g := b.Group(procmem.GEngine); g.Bytes > 0 {
		cs = append(cs, cand{"Docker engine " + fmtMem(g.Bytes), g.Bytes})
	}
	for _, p := range b.Group(procmem.GContainers).Projects {
		cs = append(cs, cand{p.Name + " " + fmtMem(int64(p.Bytes)), int64(p.Bytes)})
	}
	for _, k := range []string{procmem.GUser, procmem.GSystem, procmem.GKernel} {
		if g := b.Group(k); g.Bytes > 0 {
			cs = append(cs, cand{strings.ToLower(memGroupInfo[k].Label[:1]) + memGroupInfo[k].Label[1:] + " " + fmtMem(g.Bytes), g.Bytes})
		}
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].bytes > cs[j].bytes })
	var top []string
	for i := 0; i < len(cs) && i < 3; i++ {
		top = append(top, cs[i].label)
	}
	out := fmt.Sprintf("%s available of %s.", fmtMem(int64(b.Mem.Available)), fmtMem(int64(b.Mem.Total)))
	if len(top) > 0 {
		out += " Biggest: " + strings.Join(top, ", ") + "."
	}
	return out
}

// buildMemory turns a scan into the panel. now is used for ages.
func buildMemory(b procmem.Breakdown, errText string, configured bool, now time.Time) memView {
	v := memView{State: "ok"}
	switch {
	case !configured:
		v.State = "off"
		return v
	case b.At.IsZero() && errText != "":
		v.State, v.Error = "error", errText
		return v
	case b.At.IsZero():
		v.State = "loading"
		return v
	case errText != "":
		v.Error = errText // shown above the last good scan
	}
	total := int64(b.Mem.Total)
	v.Total = fmtMem(total)
	v.Verdict = memVerdict(b)
	v.Took = fmt.Sprintf("Scanned %d processes in %d ms, %s.", b.Procs, max(1, b.Took.Milliseconds()), ago(b.At, now))
	v.At = b.At.Format("15:04:05")
	if su := b.Mem.SwapUsed(); b.Mem.SwapTotal > 0 {
		v.Swap = fmt.Sprintf("%s of %s swap in use", fmtMem(int64(su)), fmtMem(int64(b.Mem.SwapTotal)))
	}
	var aria []string
	for _, g := range b.Groups {
		info := memGroupInfo[g.Key]
		mg := memGroup{Key: g.Key, Label: info.Label, Note: info.Note, Bytes: fmtMem(g.Bytes), Pct: fmtPct(g.Bytes, total), Negative: g.Bytes < 0}
		if g.Swap > 0 {
			mg.Swap = fmtMem(int64(g.Swap))
		}
		switch g.Key {
		case procmem.GClaude:
			mg.Count = plural(g.Count, "session", "sessions")
		case procmem.GContainers, procmem.GEngine, procmem.GUser, procmem.GSystem:
			if g.Count > 0 {
				mg.Count = plural(g.Count, "process", "processes")
			}
		}
		for _, it := range g.Items {
			r := memRow{Label: it.Label, Sub: it.Sub, Bytes: fmtMem(int64(it.Bytes)), Pct: fmtPct(int64(it.Bytes), total), PID: it.PID}
			if it.Swap > 0 {
				r.Swap = fmtMem(int64(it.Swap))
			}
			mg.Rows = append(mg.Rows, r)
		}
		for _, p := range g.Projects {
			mp := memProject{Name: p.Name, Bytes: fmtMem(int64(p.Bytes)), Pct: fmtPct(int64(p.Bytes), total)}
			for _, it := range p.Items {
				r := memRow{Label: it.Label, Bytes: fmtMem(int64(it.Bytes)), Pct: fmtPct(int64(it.Bytes), total), Sub: plural(it.Count, "process", "processes")}
				if it.Swap > 0 {
					r.Swap = fmtMem(int64(it.Swap))
				}
				mp.Rows = append(mp.Rows, r)
			}
			mg.Projects = append(mg.Projects, mp)
		}
		if g.Key == procmem.GClaude {
			for _, ss := range b.Sessions {
				ms := memSession{PID: ss.PID, Where: ss.Cwd, Name: ss.Name, Status: ss.Status, Args: ss.Args,
					Bytes: fmtMem(int64(ss.Bytes)), Pct: fmtPct(int64(ss.Bytes), total), Procs: ss.Procs}
				if ms.Where == "" {
					ms.Where = "pid " + fmt.Sprint(ss.PID)
				}
				if !ss.Start.IsZero() {
					ms.Age, ms.TS, ms.Started = ago(ss.Start, now), ss.Start.Unix(), absTime(ss.Start)
				}
				if ss.Swap > 0 {
					ms.Swap = fmtMem(int64(ss.Swap))
				}
				mg.Sessions = append(mg.Sessions, ms)
			}
		}
		v.Groups = append(v.Groups, mg)
		if g.Bytes > 0 {
			v.Segs = append(v.Segs, memSeg{Key: g.Key, Label: info.Label, Bytes: mg.Bytes, Pct: pctAttr(g.Bytes, total)})
			aria = append(aria, info.Label+" "+mg.Bytes+" ("+mg.Pct+")")
		}
	}
	v.Aria = "Memory, " + v.Total + " in total: " + strings.Join(aria, ", ") + "."
	return v
}

func (s *Server) memoryView() memView {
	b, errText := s.memSnapshot()
	return buildMemory(b, errText, s.memScan != nil, time.Now())
}

// writeMemoryMetrics: portal_memory_group_bytes{group=...} for the nine
// fixed groups (low cardinality on purpose: per-process series would
// churn with every pid) and the number of Claude Code sessions.
func writeMemoryMetrics(b *strings.Builder, m procmem.Breakdown) {
	b.WriteString("# HELP portal_memory_group_bytes Host memory by group (see Portal's Memory panel); groups sum to MemTotal.\n# TYPE portal_memory_group_bytes gauge\n")
	for _, g := range m.Groups {
		fmt.Fprintf(b, "portal_memory_group_bytes{group=\"%s\"} %d\n", g.Key, g.Bytes)
	}
	b.WriteString("# HELP portal_claude_sessions Running Claude Code CLI sessions on the host.\n# TYPE portal_claude_sessions gauge\n")
	fmt.Fprintf(b, "portal_claude_sessions %d\n", m.Group(procmem.GClaude).Count)
	b.WriteString("# HELP portal_memory_scan_seconds Duration of the last host process scan.\n# TYPE portal_memory_scan_seconds gauge\n")
	fmt.Fprintf(b, "portal_memory_scan_seconds %s\n", formatFloat(m.Took.Seconds()))
}
