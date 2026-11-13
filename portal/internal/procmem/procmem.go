// Package procmem answers "what is taking the RAM?" for the whole host.
//
// It reads the HOST's /proc (bind-mounted read-only into Portal's
// container, see docker-compose.yml) and splits MemTotal into groups that
// add up: containers, Claude Code sessions, the Docker engine, other user
// processes, system services, the kernel, page cache and free memory. What
// cannot be attributed is reported as "unaccounted", never hidden.
//
// # Which number per process
//
// /proc/<pid>/status has VmRSS = RssAnon + RssFile + RssShmem. RssFile is
// pages of files (program code, libraries, data files) that sit in the
// page cache: /proc/meminfo already counts them under Cached, and many
// processes map the same library. So a process is charged RssAnon +
// RssShmem ("own"): its heap, stacks and shared memory, which the kernel
// cannot simply drop. Summed over all processes this matches meminfo's
// AnonPages + Shmem within about 1% on this machine (anonymous pages
// shared after fork() are the only double count). PSS from smaps_rollup
// would split shared pages fairly, but Docker's default AppArmor profile
// forbids reading it for processes outside the container, so it is not
// used. Containers are charged the same way (the sum over the processes
// in their cgroup), so every group is measured with one ruler.
package procmem

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Group keys, in display order. They are also the values of the metric
// label portal_memory_group_bytes{group=...}.
const (
	GContainers  = "containers"
	GClaude      = "claude"
	GEngine      = "docker_engine"
	GUser        = "user"
	GSystem      = "system"
	GKernel      = "kernel"
	GCache       = "cache"
	GFree        = "free"
	GUnaccounted = "unaccounted"
)

// GroupOrder is every group key in display order.
var GroupOrder = []string{GContainers, GClaude, GEngine, GUser, GSystem, GKernel, GCache, GFree, GUnaccounted}

// TopN is how many processes the user and system groups list one by one.
const TopN = 8

// Meminfo is the part of /proc/meminfo the breakdown uses, in bytes.
type Meminfo struct {
	Total, Free, Available                     uint64
	Buffers, Cached, SReclaimable, SUnreclaim  uint64
	Shmem, KernelStack, PageTables, SecPageTbl uint64
	Percpu, VmallocUsed                        uint64
	SwapTotal, SwapFree                        uint64
}

// SwapUsed is swap in use.
func (m Meminfo) SwapUsed() uint64 { return sub(m.SwapTotal, m.SwapFree) }

// Container says what a container id belongs to (from the Docker list).
type Container struct {
	Name, Project string
}

// Item is one row inside a group.
type Item struct {
	Label string // "dockerd", "focal", "Slab (unreclaimable)"
	Sub   string // sanitised arguments, "12 processes", ...
	PID   int    // 0 when the row is not one process
	Bytes uint64
	Swap  uint64
	Count int // processes in this row
}

// Project is one compose project inside the containers group.
type Project struct {
	Name  string
	Bytes uint64
	Items []Item // one per container
}

// Session is one Claude Code CLI process with everything it started.
type Session struct {
	PID    int
	Start  time.Time
	Cwd    string // "~"-shortened; "" when unknown
	Name   string // from Claude's own session file, if readable
	Status string // "busy", "idle", ... from the same file; "" if unknown
	Args   string // sanitised
	Bytes  uint64 // claude itself + all descendants
	Swap   uint64
	Procs  int
}

// Group is one slice of MemTotal.
type Group struct {
	Key      string
	Bytes    int64 // signed: only "unaccounted" can be negative
	Swap     uint64
	Count    int // processes (or sessions for claude)
	Items    []Item
	Projects []Project // containers only
}

// Breakdown is one complete scan.
type Breakdown struct {
	At       time.Time
	Mem      Meminfo
	Groups   []Group // in GroupOrder
	Sessions []Session
	Procs    int           // processes looked at (kernel threads excluded)
	Took     time.Duration // wall time of the scan
}

// Group returns the group with this key.
func (b Breakdown) Group(key string) Group {
	for _, g := range b.Groups {
		if g.Key == key {
			return g
		}
	}
	return Group{Key: key}
}

// Scanner reads one /proc tree.
type Scanner struct {
	Proc        string // the host's /proc, e.g. /host/proc
	SessionsDir string // ~/.claude/sessions mounted read-only; "" = not used
	UID         int    // the owner's uid: their processes are "user", others "system"
	Home        string // shown as "~" in paths, e.g. /home/kunchu01; "" = guess /home/<x>
	ClockTick   int    // USER_HZ, 100 on every Linux build that matters
}

// proc is what one scan learns about one process.
type proc struct {
	pid, ppid, uid int
	comm           string
	own, swap      uint64
	container      string // 64-hex id, or ""
	group          string
	session        int // pid of the claude process it belongs to
}

var (
	// docker-<id>.scope (systemd cgroup driver) or /docker/<id> (cgroupfs).
	cgroupDockerRe = regexp.MustCompile(`docker[-/]([0-9a-f]{64})(?:\.scope)?`)
	engineComms    = map[string]bool{"dockerd": true, "containerd": true, "docker-proxy": true, "runc": true}
)

func isEngine(comm string) bool {
	return engineComms[comm] || strings.HasPrefix(comm, "containerd-shim")
}

// Scan reads the tree once. containers maps full container ids to names
// and compose projects. A process that exits halfway through the scan is
// simply skipped: every file is read at most once and any error drops
// only that process.
func (s *Scanner) Scan(containers map[string]Container, now time.Time) (Breakdown, error) {
	start := time.Now()
	b := Breakdown{At: now}
	mem, err := readMeminfo(filepath.Join(s.Proc, "meminfo"))
	if err != nil {
		return b, err
	}
	b.Mem = mem

	ents, err := os.ReadDir(s.Proc)
	if err != nil {
		return b, err
	}
	procs := make(map[int]*proc, len(ents))
	var buf []byte
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(s.Proc, e.Name())
		buf, err = readInto(filepath.Join(dir, "status"), buf)
		if err != nil {
			continue // gone already
		}
		p, ok := parseStatus(buf)
		if !ok {
			continue // kernel thread: no memory of its own
		}
		p.pid = pid
		if cg, err := os.ReadFile(filepath.Join(dir, "cgroup")); err == nil {
			if m := cgroupDockerRe.FindSubmatch(cg); m != nil {
				p.container = string(m[1])
			}
		}
		procs[pid] = p
	}
	b.Procs = len(procs)

	// Claude sessions: the nearest "claude" ancestor (itself included).
	// Containers are never part of a session, even if a session started
	// them: their memory belongs to the container.
	memo := map[int]int{}
	var owner func(pid, depth int) int
	owner = func(pid, depth int) int {
		if v, ok := memo[pid]; ok {
			return v
		}
		p := procs[pid]
		res := 0
		switch {
		case p == nil || depth > 64:
		case p.comm == "claude":
			res = pid
		case p.ppid != pid:
			res = owner(p.ppid, depth+1)
		}
		memo[pid] = res
		return res
	}

	groups := map[string]*Group{}
	for _, k := range GroupOrder {
		groups[k] = &Group{Key: k}
	}
	sessions := map[int]*Session{}
	projects := map[string]map[string]*Item{} // project -> container name -> row
	engine := map[string]*Item{}
	var users, system []*proc

	for _, p := range procs {
		switch {
		case p.container != "":
			p.group = GContainers
			c, ok := containers[p.container]
			if !ok {
				c = Container{Name: p.container[:12], Project: "(unknown)"}
			}
			if c.Project == "" {
				c.Project = c.Name
			}
			if projects[c.Project] == nil {
				projects[c.Project] = map[string]*Item{}
			}
			it := projects[c.Project][c.Name]
			if it == nil {
				it = &Item{Label: c.Name}
				projects[c.Project][c.Name] = it
			}
			it.Bytes += p.own
			it.Swap += p.swap
			it.Count++
		case owner(p.pid, 0) != 0:
			p.group, p.session = GClaude, owner(p.pid, 0)
			ss := sessions[p.session]
			if ss == nil {
				ss = &Session{PID: p.session}
				sessions[p.session] = ss
			}
			ss.Bytes += p.own
			ss.Swap += p.swap
			ss.Procs++
		case isEngine(p.comm):
			p.group = GEngine
			it := engine[p.comm]
			if it == nil {
				it = &Item{Label: p.comm}
				engine[p.comm] = it
			}
			it.Bytes += p.own
			it.Swap += p.swap
			it.Count++
		case p.uid == s.UID:
			p.group = GUser
			users = append(users, p)
		default:
			p.group = GSystem
			system = append(system, p)
		}
		g := groups[p.group]
		g.Bytes += int64(p.own)
		g.Swap += p.swap
		g.Count++
	}

	// Containers: projects by size, containers by size inside each.
	cg := groups[GContainers]
	for name, rows := range projects {
		pr := Project{Name: name}
		for _, it := range rows {
			pr.Bytes += it.Bytes
			pr.Items = append(pr.Items, *it)
		}
		sortItems(pr.Items)
		cg.Projects = append(cg.Projects, pr)
	}
	sort.Slice(cg.Projects, func(i, j int) bool {
		if cg.Projects[i].Bytes != cg.Projects[j].Bytes {
			return cg.Projects[i].Bytes > cg.Projects[j].Bytes
		}
		return cg.Projects[i].Name < cg.Projects[j].Name
	})

	// Claude sessions, largest first, with details for each.
	btime := readBootTime(filepath.Join(s.Proc, "stat"))
	files := s.readSessionFiles()
	for _, ss := range sessions {
		dir := filepath.Join(s.Proc, strconv.Itoa(ss.PID))
		ticks := readStartTicks(filepath.Join(dir, "stat"))
		if ticks > 0 && !btime.IsZero() {
			ss.Start = btime.Add(time.Duration(ticks) * time.Second / time.Duration(s.tick()))
		}
		ss.Args = Sanitize(readCmdline(filepath.Join(dir, "cmdline")), 60)
		if cwd, err := os.Readlink(filepath.Join(dir, "cwd")); err == nil {
			ss.Cwd = s.short(cwd)
		}
		// Claude Code's own session file: only trusted when it describes
		// THIS process (same start time), not an earlier one with a
		// recycled pid.
		if f, ok := files[ss.PID]; ok && (f.ProcStart == "" || f.ProcStart == strconv.FormatUint(ticks, 10)) {
			if ss.Cwd == "" {
				ss.Cwd = s.short(f.Cwd)
			}
			ss.Name, ss.Status = clip(f.Name, 40), clip(f.Status, 16)
		}
		b.Sessions = append(b.Sessions, *ss)
	}
	sort.Slice(b.Sessions, func(i, j int) bool {
		if b.Sessions[i].Bytes != b.Sessions[j].Bytes {
			return b.Sessions[i].Bytes > b.Sessions[j].Bytes
		}
		return b.Sessions[i].PID < b.Sessions[j].PID
	})
	groups[GClaude].Count = len(b.Sessions)

	eg := groups[GEngine]
	for _, it := range engine {
		if it.Count > 1 {
			it.Sub = strconv.Itoa(it.Count) + " processes"
		}
		eg.Items = append(eg.Items, *it)
	}
	sortItems(eg.Items)

	groups[GUser].Items = s.topItems(users)
	groups[GSystem].Items = s.topItems(system)

	// The kernel's own memory, and what Linux lends to the page cache.
	m := mem
	kg := groups[GKernel]
	kg.Items = []Item{
		{Label: "Slab, unreclaimable", Bytes: m.SUnreclaim},
		{Label: "Vmalloc", Bytes: m.VmallocUsed},
		{Label: "Page tables", Bytes: m.PageTables + m.SecPageTbl},
		{Label: "Kernel stacks", Bytes: m.KernelStack},
		{Label: "Per-CPU", Bytes: m.Percpu},
	}
	for _, it := range kg.Items {
		kg.Bytes += int64(it.Bytes)
	}
	sortItems(kg.Items)
	ch := groups[GCache]
	ch.Items = []Item{
		{Label: "Page cache", Sub: "file contents; shared memory excluded", Bytes: sub(m.Cached, m.Shmem)},
		{Label: "Reclaimable slab", Sub: "dentries, inodes", Bytes: m.SReclaimable},
		{Label: "Buffers", Sub: "block-device metadata", Bytes: m.Buffers},
	}
	for _, it := range ch.Items {
		ch.Bytes += int64(it.Bytes)
	}
	groups[GFree].Bytes = int64(m.Free)

	var sum int64
	for _, k := range GroupOrder[:len(GroupOrder)-1] {
		sum += groups[k].Bytes
	}
	groups[GUnaccounted].Bytes = int64(m.Total) - sum

	for _, k := range GroupOrder {
		b.Groups = append(b.Groups, *groups[k])
	}
	b.Took = time.Since(start)
	return b, nil
}

func (s *Scanner) tick() int {
	if s.ClockTick > 0 {
		return s.ClockTick
	}
	return 100
}

// topItems lists the TopN biggest processes, then one row for the rest.
// Only those rows pay for reading a command line.
func (s *Scanner) topItems(list []*proc) []Item {
	sort.Slice(list, func(i, j int) bool {
		if list[i].own != list[j].own {
			return list[i].own > list[j].own
		}
		return list[i].pid < list[j].pid
	})
	var out []Item
	var rest Item
	for i, p := range list {
		if i < TopN {
			args := readCmdline(filepath.Join(s.Proc, strconv.Itoa(p.pid), "cmdline"))
			out = append(out, Item{Label: p.comm, Sub: Sanitize(args, 60), PID: p.pid, Bytes: p.own, Swap: p.swap, Count: 1})
			continue
		}
		rest.Bytes += p.own
		rest.Swap += p.swap
		rest.Count++
	}
	if rest.Count > 0 {
		rest.Label = strconv.Itoa(rest.Count) + " more processes"
		out = append(out, rest)
	}
	return out
}

func sortItems(list []Item) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Bytes != list[j].Bytes {
			return list[i].Bytes > list[j].Bytes
		}
		return list[i].Label < list[j].Label
	})
}

// short shows the owner's home as "~".
func (s *Scanner) short(p string) string {
	if p == "" {
		return ""
	}
	home := s.Home
	if home == "" {
		if m := homeRe.FindString(p); m != "" {
			home = m
		}
	}
	if home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		p = "~" + p[len(home):]
	}
	return clip(p, 60)
}

var homeRe = regexp.MustCompile(`^/home/[^/]+`)

// ---- file parsers ----

// readInto reads a small file, reusing buf so a scan of ~400 processes
// does not allocate a fresh buffer for each. At most 64 KB is read.
func readInto(path string, buf []byte) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return buf, err
	}
	defer f.Close()
	buf = buf[:0]
	for len(buf) < 64<<10 {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)] // grow
		}
		n, err := f.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF || (err == nil && n == 0) {
			return buf, nil
		}
		if err != nil { // e.g. ESRCH: the process exited while we read
			return buf, err
		}
	}
	return buf, nil
}

// parseStatus reads Name, PPid, Uid (real), RssAnon, RssShmem and VmSwap.
// A process without a VmRSS line is a kernel thread (or a zombie): it has
// no user memory and is skipped (ok = false).
func parseStatus(b []byte) (*proc, bool) {
	p := &proc{uid: -1}
	hasRSS := false
	for len(b) > 0 {
		var line []byte
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			line, b = b, nil
		}
		k, v, ok := bytes.Cut(line, []byte{':'})
		if !ok {
			continue
		}
		v = bytes.TrimSpace(v)
		switch string(k) {
		case "Name":
			p.comm = string(v)
		case "PPid":
			p.ppid, _ = strconv.Atoi(string(v))
		case "Uid":
			if f := bytes.Fields(v); len(f) > 0 {
				p.uid, _ = strconv.Atoi(string(f[0]))
			}
		case "VmRSS":
			hasRSS = true
		case "RssAnon", "RssShmem":
			p.own += kb(v)
		case "VmSwap":
			p.swap = kb(v)
		}
	}
	return p, hasRSS
}

// kb parses "1234 kB" into bytes.
func kb(v []byte) uint64 {
	f := bytes.Fields(v)
	if len(f) == 0 {
		return 0
	}
	n, _ := strconv.ParseUint(string(f[0]), 10, 64)
	return n * 1024
}

func readMeminfo(path string) (Meminfo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Meminfo{}, err
	}
	var m Meminfo
	fields := map[string]*uint64{
		"MemTotal": &m.Total, "MemFree": &m.Free, "MemAvailable": &m.Available,
		"Buffers": &m.Buffers, "Cached": &m.Cached, "SReclaimable": &m.SReclaimable,
		"SUnreclaim": &m.SUnreclaim, "Shmem": &m.Shmem, "KernelStack": &m.KernelStack,
		"PageTables": &m.PageTables, "SecPageTables": &m.SecPageTbl, "Percpu": &m.Percpu,
		"VmallocUsed": &m.VmallocUsed, "SwapTotal": &m.SwapTotal, "SwapFree": &m.SwapFree,
	}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		k, v, ok := bytes.Cut(line, []byte{':'})
		if !ok {
			continue
		}
		if p := fields[string(k)]; p != nil {
			*p = kb(v)
		}
	}
	if m.Total == 0 {
		return m, errors.New("meminfo: no MemTotal")
	}
	return m, nil
}

// readBootTime reads "btime <unix seconds>" from /proc/stat.
func readBootTime(path string) time.Time {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if v, ok := bytes.CutPrefix(line, []byte("btime ")); ok {
			if n, err := strconv.ParseInt(string(bytes.TrimSpace(v)), 10, 64); err == nil {
				return time.Unix(n, 0)
			}
		}
	}
	return time.Time{}
}

// readStartTicks returns field 22 of /proc/<pid>/stat (start time in clock
// ticks after boot). The command name (field 2) may contain spaces and
// parentheses, so counting starts after the LAST ')'.
func readStartTicks(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0
	}
	f := bytes.Fields(b[i+1:])
	if len(f) < 20 { // fields 3..22 -> index 0..19
		return 0
	}
	n, _ := strconv.ParseUint(string(f[19]), 10, 64)
	return n
}

// readCmdline returns argv (NUL-separated in the file), at most 4 KB.
func readCmdline(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b := make([]byte, 4096)
	n, _ := f.Read(b)
	b = bytes.TrimRight(b[:n], "\x00")
	if len(b) == 0 {
		return nil
	}
	parts := bytes.Split(b, []byte{0})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}

// sessionFile is the part of ~/.claude/sessions/<pid>.json used here.
type sessionFile struct {
	PID       int    `json:"pid"`
	Cwd       string `json:"cwd"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	ProcStart string `json:"procStart"`
}

var sessionFileRe = regexp.MustCompile(`^[0-9]{1,9}\.json$`)

// readSessionFiles reads Claude Code's per-process session files. Only
// files named <digits>.json and smaller than 16 KB are opened; anything
// else in the directory (such as key files) is never read.
func (s *Scanner) readSessionFiles() map[int]sessionFile {
	out := map[int]sessionFile{}
	if s.SessionsDir == "" {
		return out
	}
	ents, err := os.ReadDir(s.SessionsDir)
	if err != nil {
		return out
	}
	for _, e := range ents {
		if !e.Type().IsRegular() || !sessionFileRe.MatchString(e.Name()) {
			continue
		}
		if info, err := e.Info(); err != nil || info.Size() > 16<<10 {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.SessionsDir, e.Name()))
		if err != nil {
			continue
		}
		var f sessionFile
		if json.Unmarshal(b, &f) != nil || f.PID <= 0 || strconv.Itoa(f.PID)+".json" != e.Name() {
			continue
		}
		out[f.PID] = f
	}
	return out
}

// ---- argument sanitising ----

var (
	// A flag whose NEXT argument is a secret: --token abc, -p hunter2.
	secretFlagRe = regexp.MustCompile(`(?i)^-{1,2}[a-z0-9_-]*(token|passwd|pass|password|pwd|secret|key|auth|authorization|credentials?|cookie|header)$|^-p$|^-H$|^bearer$|^basic$`)
	// NAME=value where NAME looks secret, also --password=value.
	secretKVRe = regexp.MustCompile(`(?i)^(-{0,2}[a-z0-9_.-]*(token|passw|pwd|secret|key|auth|credential|cookie|session|dsn)[a-z0-9_.-]*)=`)
	// Words that make the whole argument suspect.
	secretWordRe = regexp.MustCompile(`(?i)authorization|bearer\s|basic\s`)
	// Well-known token shapes.
	tokenShapeRe = regexp.MustCompile(`^(sk-|sk_|tk_|ghp_|gho_|github_pat_|glpat-|xox[abp]-|AKIA)[A-Za-z0-9_\-]{6,}`)
	// user:password@ inside a URL.
	urlCredRe = regexp.MustCompile(`(://[^/@:\s]+):[^/@\s]+@`)
)

// Sanitize turns argv into one short, display-safe line: the program's
// base name, then its arguments with anything secret-looking replaced by
// "***", cut to max runes. Command lines are world-readable on Linux, but
// tools still pass tokens on them; this keeps those off the dashboard.
func Sanitize(argv []string, max int) string {
	if len(argv) == 0 {
		return ""
	}
	out := make([]string, 0, len(argv))
	out = append(out, filepath.Base(argv[0]))
	redactNext := false
	for _, a := range argv[1:] {
		switch {
		case redactNext:
			a, redactNext = "***", false
		case secretFlagRe.MatchString(a):
			redactNext = true
		case secretKVRe.MatchString(a):
			a = secretKVRe.FindStringSubmatch(a)[1] + "=***"
		case secretWordRe.MatchString(a), tokenShapeRe.MatchString(a):
			a = "***"
		default:
			a = urlCredRe.ReplaceAllString(a, "$1:***@")
		}
		out = append(out, a)
	}
	return clip(strings.Join(out, " "), max)
}

// clip cuts s to n runes, with an ellipsis, and drops control characters.
func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func sub(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return 0
}
