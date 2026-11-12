// Package sysinfo reads host statistics straight from the kernel's
// /proc and /sys pseudo-files. No external tools are run.
//
// Portal's container shares the host network namespace and Docker does
// not virtualise /proc/meminfo, /proc/stat or /sys/class, so the numbers
// read here are the laptop's, not the container's.
package sysinfo

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Snapshot is one reading of the host. A zero/false "OK" field means that
// sensor is not available on this machine.
type Snapshot struct {
	At time.Time

	CPUPercent float64 // busy share of all cores since the previous snapshot
	Cores      int
	Load1      float64

	MemTotal, MemUsed   uint64 // bytes; used = total - available
	DiskTotal, DiskUsed uint64 // bytes, for the filesystem holding the data dir

	TempC  float64
	TempOK bool

	Uptime time.Duration

	WiFiIface   string
	WiFiDBm     int // signal level, e.g. -52
	WiFiQuality int // link quality 0..70
	WiFiOK      bool

	BatteryPercent int
	BatteryStatus  string // Charging, Discharging, Full, Not charging, Unknown
	ACOnline       bool
	BatteryOK      bool
}

// Collector produces snapshots. CPU usage is a rate, so it has to
// remember the previous counters between calls.
type Collector struct {
	Proc     string // normally /proc
	Sys      string // normally /sys
	DiskPath string // any path on the filesystem to report

	mu                  sync.Mutex
	prevBusy, prevTotal uint64
}

// New returns a collector for the real /proc and /sys.
func New(diskPath string) *Collector {
	return &Collector{Proc: "/proc", Sys: "/sys", DiskPath: diskPath}
}

// Snapshot takes a reading.
func (c *Collector) Snapshot() Snapshot {
	s := Snapshot{At: time.Now()}
	c.cpu(&s)
	c.mem(&s)
	c.disk(&s)
	c.temp(&s)
	c.uptime(&s)
	c.wifi(&s)
	c.battery(&s)
	return s
}

// cpu reads the first line of /proc/stat:
//
//	cpu  user nice system idle iowait irq softirq steal ...
//
// The values are ticks since boot, summed over all cores. Usage is how
// much of the tick increase since the last reading was not idle.
func (c *Collector) cpu(s *Snapshot) {
	f, err := os.Open(filepath.Join(c.Proc, "stat"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "cpu" && len(fields) >= 9 {
			var total, idle uint64
			for i, v := range fields[1:9] {
				n, _ := strconv.ParseUint(v, 10, 64)
				total += n
				if i == 3 || i == 4 { // idle + iowait
					idle += n
				}
			}
			busy := total - idle
			c.mu.Lock()
			if c.prevTotal != 0 && total > c.prevTotal {
				s.CPUPercent = 100 * float64(busy-c.prevBusy) / float64(total-c.prevTotal)
			}
			c.prevBusy, c.prevTotal = busy, total
			c.mu.Unlock()
		} else if strings.HasPrefix(fields[0], "cpu") {
			s.Cores++
		}
	}
	if b, err := os.ReadFile(filepath.Join(c.Proc, "loadavg")); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			s.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
}

// mem uses MemAvailable, the kernel's own estimate of what could be
// handed to programs without swapping. "Free" would be misleading: Linux
// fills spare RAM with disk cache that it gives back on demand.
func (c *Collector) mem(s *Snapshot) {
	f, err := os.Open(filepath.Join(c.Proc, "meminfo"))
	if err != nil {
		return
	}
	defer f.Close()
	var total, avail uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			avail = kb * 1024
		}
	}
	s.MemTotal = total
	if total >= avail {
		s.MemUsed = total - avail
	}
}

// disk asks the kernel about the filesystem that holds DiskPath.
func (c *Collector) disk(s *Snapshot) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(c.DiskPath, &st); err != nil {
		return
	}
	bs := uint64(st.Bsize)
	s.DiskTotal = st.Blocks * bs
	// Bavail (not Bfree) excludes the blocks reserved for root, matching
	// what "df" reports as available.
	s.DiskUsed = s.DiskTotal - st.Bavail*bs
}

// temp picks the CPU package sensor when there is one, otherwise the
// hottest thermal zone. Values are in thousandths of a degree.
func (c *Collector) temp(s *Snapshot) {
	zones, _ := filepath.Glob(filepath.Join(c.Sys, "class/thermal/thermal_zone*"))
	best := -1
	var hottest float64
	for _, z := range zones {
		milli, err := strconv.Atoi(readTrim(filepath.Join(z, "temp")))
		if err != nil || milli <= 0 {
			continue
		}
		t := float64(milli) / 1000
		rank := 0
		switch readTrim(filepath.Join(z, "type")) {
		case "x86_pkg_temp":
			rank = 3
		case "TCPU", "cpu-thermal", "coretemp":
			rank = 2
		case "acpitz":
			rank = 1
		}
		if rank > best || (rank == best && t > hottest) {
			best, hottest = rank, t
		}
	}
	if best >= 0 {
		s.TempC, s.TempOK = hottest, true
	}
}

func (c *Collector) uptime(s *Snapshot) {
	if f := strings.Fields(readTrim(filepath.Join(c.Proc, "uptime"))); len(f) > 0 {
		secs, _ := strconv.ParseFloat(f[0], 64)
		s.Uptime = time.Duration(secs * float64(time.Second))
	}
}

// wifi parses /proc/net/wireless, which has two header lines and then:
//
//	wlp0s20f3: 0000   62.  -48.  -256  ...
//	           status link level noise
func (c *Collector) wifi(s *Snapshot) {
	f, err := os.Open(filepath.Join(c.Proc, "net/wireless"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for line := 0; sc.Scan(); line++ {
		if line < 2 {
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		q, _ := strconv.ParseFloat(strings.TrimSuffix(fields[2], "."), 64)
		lvl, _ := strconv.ParseFloat(strings.TrimSuffix(fields[3], "."), 64)
		s.WiFiIface = strings.TrimSuffix(fields[0], ":")
		s.WiFiQuality, s.WiFiDBm, s.WiFiOK = int(q), int(lvl), true
		return
	}
}

// battery reads /sys/class/power_supply. Each entry has a "type" file:
// "Battery" entries have capacity (percent) and status; "Mains" entries
// have online (1 when the charger is plugged in).
func (c *Collector) battery(s *Snapshot) {
	entries, _ := filepath.Glob(filepath.Join(c.Sys, "class/power_supply/*"))
	for _, e := range entries {
		switch readTrim(filepath.Join(e, "type")) {
		case "Battery":
			if s.BatteryOK {
				continue // first battery wins
			}
			if pct, err := strconv.Atoi(readTrim(filepath.Join(e, "capacity"))); err == nil {
				s.BatteryPercent, s.BatteryOK = pct, true
				s.BatteryStatus = readTrim(filepath.Join(e, "status"))
			}
		case "Mains":
			if readTrim(filepath.Join(e, "online")) == "1" {
				s.ACOnline = true
			}
		}
	}
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
