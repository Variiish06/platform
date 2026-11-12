package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"portal/internal/docker"
)

// dockerAPI is the part of the Docker client the server uses. An
// interface so tests can substitute a fake engine.
type dockerAPI interface {
	List(ctx context.Context) ([]docker.Container, error)
	Stats(ctx context.Context, id string) (docker.Stats, error)
	Restart(ctx context.Context, id string) error
}

// Timeouts for one collection round. The whole round must finish well
// within the 10 s fast cadence even if one container's stats hang.
const (
	dockerListTimeout  = 5 * time.Second
	dockerStatsTimeout = 4 * time.Second
	// containerMetricsMaxAge: after this long without a successful round,
	// /metrics stops reporting per-container series rather than repeating
	// stale "1 = running" values (an absent series is honest; alerts can
	// use absent() for it, and portal_docker_up says why).
	containerMetricsMaxAge = 2 * time.Minute
)

// collectContainers lists every container and reads resource usage for the
// running ones. It runs in the background (see startBackground) every 30 s,
// or every 10 s while a dashboard is open, and after a restart. Handlers
// and /metrics only ever read the result (s.containers), so a slow Docker
// can never slow down a page load or a scrape.
func (s *Server) collectContainers(ctx context.Context) {
	// One round at a time: the previous-sample map below is not shared.
	s.collectMu.Lock()
	defer s.collectMu.Unlock()

	lctx, cancel := context.WithTimeout(ctx, dockerListTimeout)
	list, err := s.docker.List(lctx)
	cancel()
	if err != nil {
		s.mu.Lock()
		if s.dockerErr == "" {
			log.Printf("docker list: %v", err) // once per outage
		}
		s.dockerErr = "Can't reach Docker. Is the socket proxy running?"
		s.mu.Unlock()
		return
	}

	// Fan out the stats calls to a few workers. A buffered channel of
	// indexes is the queue; each worker writes only its own slots of
	// results, so no lock is needed.
	type result struct {
		st  docker.Stats
		err error
	}
	results := make([]result, len(list))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range docker.StatsWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				sctx, cancel := context.WithTimeout(ctx, dockerStatsTimeout)
				results[i].st, results[i].err = s.docker.Stats(sctx, list[i].ID)
				cancel()
			}
		}()
	}
	for i, c := range list {
		if c.Running() {
			jobs <- i
		}
	}
	close(jobs)
	wg.Wait()

	// CPU % needs two samples. Docker's one-shot answer has no previous
	// sample (precpu_stats is empty), so we remember our own from the last
	// round, keyed by container ID. The new map only holds containers that
	// still exist, which also forgets removed ones.
	prev := make(map[string]docker.CPUSample, len(list))
	failed := 0
	for i := range list {
		c := &list[i]
		if !c.Running() {
			continue
		}
		r := results[i]
		if r.err != nil {
			failed++
			if p, ok := s.cpuPrev[c.ID]; ok {
				prev[c.ID] = p // keep the old sample for next time
			}
			continue
		}
		c.MemBytes, c.MemOK = r.st.Memory, r.st.MemOK
		switch {
		case r.st.PreCPU.Valid(): // Docker gave us both samples (not one-shot)
			c.CPUPercent, c.CPUOK = docker.CPUPercent(r.st.PreCPU, r.st.CPU)
		default:
			if p, ok := s.cpuPrev[c.ID]; ok {
				c.CPUPercent, c.CPUOK = docker.CPUPercent(p, r.st.CPU)
			}
		}
		if r.st.CPU.Valid() {
			prev[c.ID] = r.st.CPU
		}
	}
	s.cpuPrev = prev
	if failed > 0 {
		log.Printf("docker stats: %d of %d running containers failed", failed, len(prev))
	}

	s.mu.Lock()
	s.containers, s.dockerErr, s.containersAt = list, "", time.Now()
	s.mu.Unlock()
}

// containerSnapshot returns the cached list, and whether it is recent
// enough to export as metrics.
func (s *Server) containerSnapshot() ([]docker.Container, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.containers, !s.containersAt.IsZero() && time.Since(s.containersAt) < containerMetricsMaxAge
}

// writeContainerMetrics renders the per-container series. Separate from
// the handler so the exact text can be tested.
//
// portal_container_up exists for EVERY container (stopped ones as 0), so
// "a container stopped" is a value change an alert rule can see, not a
// series silently disappearing. CPU and memory only exist for running
// containers that answered the stats call.
func writeContainerMetrics(b *strings.Builder, list []docker.Container) {
	b.WriteString("# HELP portal_container_up Whether the container is running (1) or not (0), for every container.\n# TYPE portal_container_up gauge\n")
	for _, c := range list {
		up := 0
		if c.Running() {
			up = 1
		}
		fmt.Fprintf(b, "portal_container_up{name=\"%s\"} %d\n", escapeLabel(c.Name), up)
	}
	b.WriteString("# HELP portal_container_cpu_percent CPU used by the container between the last two samples; 100 = one core.\n# TYPE portal_container_cpu_percent gauge\n")
	for _, c := range list {
		if c.Running() && c.CPUOK {
			fmt.Fprintf(b, "portal_container_cpu_percent{name=\"%s\"} %s\n", escapeLabel(c.Name), formatFloat(c.CPUPercent))
		}
	}
	b.WriteString("# HELP portal_container_memory_bytes Memory used by the container, excluding reclaimable page cache (as docker stats).\n# TYPE portal_container_memory_bytes gauge\n")
	for _, c := range list {
		if c.Running() && c.MemOK {
			fmt.Fprintf(b, "portal_container_memory_bytes{name=\"%s\"} %d\n", escapeLabel(c.Name), c.MemBytes)
		}
	}
}
