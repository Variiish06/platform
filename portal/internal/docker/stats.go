package docker

import (
	"context"
	"net/http"
)

// statsWorkers is how many stats requests the collector runs at once.
// Each one makes dockerd read a handful of cgroup files; four in parallel
// keeps a round of ~20 containers well under a second without a burst of
// 20 simultaneous requests through the socket proxy.
const statsWorkers = 4

// StatsWorkers exposes the limit to the collector in the server package.
const StatsWorkers = statsWorkers

// CPUSample is one reading of a container's CPU counters. Both numbers
// only ever grow (they are totals since boot / since the container
// started), so usage is always computed from the DIFFERENCE of two
// samples: "how much CPU time did the container get, out of how much
// CPU time passed on the whole machine, between then and now".
type CPUSample struct {
	Total  uint64 // cpu_stats.cpu_usage.total_usage: ns of CPU used by the container
	System uint64 // cpu_stats.system_cpu_usage: ns of CPU time elapsed on the host, all cores summed
	CPUs   int    // online CPUs
}

// Valid reports whether the sample carries counters at all. With
// one-shot=true Docker leaves precpu_stats empty (all zeros).
func (s CPUSample) Valid() bool { return s.System > 0 }

// Stats is the part of GET /containers/{id}/stats we use.
type Stats struct {
	CPU    CPUSample // now
	PreCPU CPUSample // Docker's previous reading; zero with one-shot=true
	Memory uint64    // usage minus reclaimable page cache, as "docker stats" shows
	MemOK  bool
}

// rawStats mirrors the JSON. Only the fields we read are declared; the
// decoder ignores the rest.
type rawStats struct {
	CPUStats    rawCPU `json:"cpu_stats"`
	PreCPUStats rawCPU `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}

type rawCPU struct {
	CPUUsage struct {
		TotalUsage  uint64   `json:"total_usage"`
		PercpuUsage []uint64 `json:"percpu_usage"` // cgroup v1 only
	} `json:"cpu_usage"`
	SystemUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs  int    `json:"online_cpus"`
}

func (r rawCPU) sample() CPUSample {
	n := r.OnlineCPUs
	if n == 0 { // old engines: count the per-CPU array instead
		n = len(r.CPUUsage.PercpuUsage)
	}
	return CPUSample{Total: r.CPUUsage.TotalUsage, System: r.SystemUsage, CPUs: n}
}

// parse turns the raw document into Stats. Separate from the HTTP call
// so the maths can be unit-tested with canned JSON.
func (r rawStats) parse() Stats {
	st := Stats{CPU: r.CPUStats.sample(), PreCPU: r.PreCPUStats.sample()}
	usage := r.MemoryStats.Usage
	if usage > 0 {
		// "usage" includes page cache the kernel can drop at any time.
		// Subtract it (cgroup v2: inactive_file; v1: total_inactive_file),
		// exactly as the docker CLI does.
		cache, ok := r.MemoryStats.Stats["inactive_file"]
		if !ok {
			cache = r.MemoryStats.Stats["total_inactive_file"]
		}
		if cache < usage {
			usage -= cache
		}
		st.Memory, st.MemOK = usage, true
	}
	return st
}

// Stats reads one sample of a container's resource usage.
//
// one-shot=true makes Docker answer immediately with a single reading.
// Without it, Docker waits about a second to take a second reading so it
// can fill precpu_stats, which would make a round over 20 containers
// slow. We keep the previous sample ourselves instead (see CPUPercent).
func (c *Client) Stats(ctx context.Context, id string) (Stats, error) {
	var raw rawStats
	if err := c.do(ctx, http.MethodGet, "/containers/"+id+"/stats?stream=false&one-shot=true", &raw); err != nil {
		return Stats{}, err
	}
	return raw.parse(), nil
}

// CPUPercent computes CPU usage between two samples the way "docker stats"
// does:
//
//	container CPU time used      host CPU time elapsed
//	(cur.Total - prev.Total)  /  (cur.System - prev.System)  x  online CPUs  x 100
//
// system_cpu_usage counts every core, so the ratio alone is "share of the
// whole machine"; multiplying by the number of CPUs rescales it so that
// 100% means one full core (a busy 4-thread job on a 4-core box shows
// ~400%, the same as docker stats and top).
//
// ok is false when the delta is meaningless: no previous sample, no time
// passed, or the container's counter went DOWN (it restarted, which
// resets its cgroup counters).
func CPUPercent(prev, cur CPUSample) (pct float64, ok bool) {
	if !prev.Valid() || !cur.Valid() || cur.System <= prev.System || cur.Total < prev.Total {
		return 0, false
	}
	cpus := cur.CPUs
	if cpus <= 0 {
		cpus = 1
	}
	return float64(cur.Total-prev.Total) / float64(cur.System-prev.System) * float64(cpus) * 100, true
}
