package procmem

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const ctr = "3d6125f9550a466c5797d9274e5b16519eec146ecfea520a9d1e4bc58be4c046"

type fakeProc struct {
	pid, ppid, uid int
	comm           string
	anon, shm, swp int // kB
	kernel         bool
	cgroup         string
	cmdline        []string
	start          uint64
	noStatus       bool // vanished between ReadDir and the read
	unreadable     bool // cgroup and cmdline mode 000
}

func writeTree(t *testing.T, procs []fakeProc) string {
	t.Helper()
	root := t.TempDir()
	mk := func(p, s string) {
		if err := os.WriteFile(filepath.Join(root, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("meminfo", `MemTotal:        8000000 kB
MemFree:         1000000 kB
MemAvailable:    5000000 kB
Buffers:          300000 kB
Cached:          3000000 kB
SwapTotal:       4000000 kB
SwapFree:        3000000 kB
Shmem:            100000 kB
SReclaimable:     500000 kB
SUnreclaim:       200000 kB
KernelStack:       10000 kB
PageTables:        20000 kB
SecPageTables:      1000 kB
Percpu:             9000 kB
VmallocUsed:       60000 kB
`)
	mk("stat", "cpu  1 2 3 4\nbtime 1794500000\n")
	mk("self", "not a pid")
	for _, p := range procs {
		dir := filepath.Join(root, fmt.Sprint(p.pid))
		os.Mkdir(dir, 0o755)
		if p.noStatus {
			continue
		}
		st := fmt.Sprintf("Name:\t%s\nUmask:\t0022\nState:\tS (sleeping)\nPPid:\t%d\nUid:\t%d\t%d\t%d\t%d\n", p.comm, p.ppid, p.uid, p.uid, p.uid, p.uid)
		if !p.kernel {
			st += fmt.Sprintf("VmRSS:\t%d kB\nRssAnon:\t%d kB\nRssFile:\t99999 kB\nRssShmem:\t%d kB\nVmSwap:\t%d kB\n", p.anon+p.shm+99999, p.anon, p.shm, p.swp)
		}
		os.WriteFile(filepath.Join(dir, "status"), []byte(st), 0o644)
		cg := p.cgroup
		if cg == "" {
			cg = "0::/user.slice/user-1000.slice/session-1.scope\n"
		}
		os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cg), 0o644)
		os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(p.cmdline, "\x00")+"\x00"), 0o644)
		os.WriteFile(filepath.Join(dir, "stat"), []byte(fmt.Sprintf("%d (%s) S %d 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 0", p.pid, p.comm, p.ppid, p.start)), 0o644)
		if p.unreadable {
			os.Chmod(filepath.Join(dir, "cgroup"), 0)
			os.Chmod(filepath.Join(dir, "cmdline"), 0)
		}
	}
	return root
}

func TestScanClassifiesAndSums(t *testing.T) {
	root := writeTree(t, []fakeProc{
		{pid: 1, comm: "systemd", uid: 0, anon: 8000},
		{pid: 2, comm: "kthreadd", uid: 0, kernel: true},
		{pid: 40, ppid: 2, comm: "kworker/0:1", uid: 0, kernel: true},
		{pid: 100, ppid: 1, comm: "dockerd", uid: 0, anon: 400000},
		{pid: 101, ppid: 1, comm: "containerd-shim", uid: 0, anon: 5000},
		{pid: 102, ppid: 1, comm: "containerd-shim", uid: 0, anon: 5000},
		{pid: 200, ppid: 101, comm: "postgres", uid: 70, anon: 30000, shm: 20000, cgroup: "0::/system.slice/docker-" + ctr + ".scope\n"},
		{pid: 201, ppid: 200, comm: "postgres", uid: 70, anon: 10000, cgroup: "0::/../docker-" + ctr + ".scope\n"},
		{pid: 300, ppid: 1, comm: "tmux: server", uid: 1000, anon: 3000},
		{pid: 301, ppid: 300, comm: "claude", uid: 1000, anon: 300000, swp: 1000, start: 1000,
			cmdline: []string{"claude", "--model", "x", "--api-key", "sk-SECRET"}},
		{pid: 302, ppid: 301, comm: "node", uid: 1000, anon: 50000, cmdline: []string{"node", "mcp.js"}},
		{pid: 303, ppid: 302, comm: "bash", uid: 1000, anon: 2000},
		{pid: 304, ppid: 303, comm: "docker", uid: 1000, anon: 1000},
		// A second session started from the first one's shell.
		{pid: 310, ppid: 303, comm: "claude", uid: 1000, anon: 100000, start: 2000},
		{pid: 400, ppid: 1, comm: "uvicorn", uid: 1000, anon: 60000, cmdline: []string{"/usr/bin/uvicorn", "app:app", "--password", "hunter2"}},
		{pid: 500, ppid: 1, comm: "tailscaled", uid: 0, anon: 40000},
		{pid: 600, ppid: 1, comm: "gone", uid: 1000, noStatus: true},
		{pid: 700, ppid: 1, comm: "secretive", uid: 0, anon: 1000, unreadable: true},
	})
	sess := t.TempDir()
	os.WriteFile(filepath.Join(sess, "301.json"), []byte(`{"pid":301,"cwd":"/home/u/platform","name":"plat","status":"idle","procStart":"1000"}`), 0o644)
	os.WriteFile(filepath.Join(sess, "310.json"), []byte(`{"pid":310,"cwd":"/home/u/old","procStart":"999"}`), 0o644) // pid reused: ignored
	os.WriteFile(filepath.Join(sess, "301.abc.key"), []byte(`SECRET`), 0o600)

	s := &Scanner{Proc: root, SessionsDir: sess, UID: 1000, Home: "/home/u"}
	b, err := s.Scan(map[string]Container{ctr: {Name: "dalal-postgres-1", Project: "dalal"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	g := func(k string) Group { return b.Group(k) }
	const K = 1024
	if v := g(GContainers).Bytes; v != (30000+20000+10000)*K {
		t.Errorf("containers = %d", v)
	}
	if p := g(GContainers).Projects; len(p) != 1 || p[0].Name != "dalal" || p[0].Items[0].Label != "dalal-postgres-1" || p[0].Items[0].Count != 2 {
		t.Errorf("projects = %+v", p)
	}
	if v := g(GClaude).Bytes; v != (300000+50000+2000+1000+100000)*K {
		t.Errorf("claude = %d", v)
	}
	if len(b.Sessions) != 2 || b.Sessions[0].PID != 301 || b.Sessions[0].Procs != 4 || b.Sessions[1].PID != 310 {
		t.Fatalf("sessions = %+v", b.Sessions)
	}
	s0 := b.Sessions[0]
	if s0.Cwd != "~/platform" || s0.Status != "idle" || s0.Name != "plat" || s0.Swap != 1000*K {
		t.Errorf("session details = %+v", s0)
	}
	if want := time.Unix(1794500000+10, 0); !s0.Start.Equal(want) {
		t.Errorf("start = %v, want %v", s0.Start, want)
	}
	if strings.Contains(s0.Args, "SECRET") || !strings.Contains(s0.Args, "--api-key ***") {
		t.Errorf("args not redacted: %q", s0.Args)
	}
	if b.Sessions[1].Cwd != "" {
		t.Errorf("stale session file trusted: %+v", b.Sessions[1])
	}
	if v := g(GEngine).Bytes; v != 410000*K {
		t.Errorf("engine = %d", v)
	}
	if it := g(GEngine).Items; len(it) != 2 || it[1].Label != "containerd-shim" || it[1].Sub != "2 processes" {
		t.Errorf("engine items = %+v", it)
	}
	if v := g(GUser).Bytes; v != 63000*K {
		t.Errorf("user = %d", v)
	}
	if it := g(GUser).Items; it[0].Label != "uvicorn" || strings.Contains(it[0].Sub, "hunter2") || it[0].Sub != "uvicorn app:app --password ***" {
		t.Errorf("user items = %+v", it)
	}
	if v := g(GSystem).Bytes; v != (8000+40000+1000)*K {
		t.Errorf("system = %d (unreadable cgroup must still count)", v)
	}
	if v := g(GKernel).Bytes; v != (200000+10000+20000+1000+9000+60000)*K {
		t.Errorf("kernel = %d", v)
	}
	if v := g(GCache).Bytes; v != (3000000-100000+300000+500000)*K {
		t.Errorf("cache = %d", v)
	}
	if g(GFree).Bytes != 1000000*K || b.Mem.SwapUsed() != 1000000*K {
		t.Error("free/swap")
	}
	var sum int64
	for _, gr := range b.Groups {
		sum += gr.Bytes
	}
	if sum != 8000000*K {
		t.Errorf("groups sum to %d, not MemTotal", sum)
	}
	if b.Procs != 15 { // 18 entries - 2 kernel threads - 1 vanished
		t.Errorf("procs = %d", b.Procs)
	}
}

func TestScanWithoutProc(t *testing.T) {
	s := &Scanner{Proc: filepath.Join(t.TempDir(), "nope")}
	if _, err := s.Scan(nil, time.Now()); err == nil {
		t.Fatal("want error")
	}
}

func TestTopNAndRest(t *testing.T) {
	var ps []fakeProc
	for i := range 12 {
		ps = append(ps, fakeProc{pid: 1000 + i, ppid: 1, comm: fmt.Sprintf("p%d", i), uid: 1000, anon: 1000 * (i + 1)})
	}
	b, err := (&Scanner{Proc: writeTree(t, ps), UID: 1000}).Scan(nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	it := b.Group(GUser).Items
	if len(it) != TopN+1 || it[0].Label != "p11" || it[TopN].Label != "4 more processes" || it[TopN].Bytes != (1+2+3+4)*1000*1024 {
		t.Fatalf("%+v", it)
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"/usr/bin/curl -H Authorization:x https://u:pw@h/x": "curl -H *** https://u:***@h/x",
		"app --token=abc DB_PASSWORD=x PORT=8080":           "app --token=*** DB_PASSWORD=*** PORT=8080",
		"redis-server -p 6379 --requirepass x":              "redis-server -p *** --requirepass ***",
		"tool Bearer abc ghp_abcdefghijk":                   "tool Bearer *** ***",
		"x --github-token y":                                "x --github-token ***",
	}
	for in, want := range cases {
		if got := Sanitize(strings.Fields(in), 200); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Sanitize([]string{"a", strings.Repeat("b", 100)}, 20); len([]rune(got)) != 20 || !strings.HasSuffix(got, "…") {
		t.Errorf("clip: %q", got)
	}
	if Sanitize(nil, 10) != "" || Sanitize([]string{"a\nb"}, 10) != "a b" {
		t.Error("edge cases")
	}
}
