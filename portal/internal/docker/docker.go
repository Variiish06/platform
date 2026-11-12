// Package docker is a minimal client for the four Docker Engine API
// calls the dashboard needs: list containers, read stats, restart.
//
// It never touches /var/run/docker.sock. It talks to a unix socket
// published by the docker-socket-proxy container, which forwards only
// GET list/stats and POST restart, and rejects everything else. So even
// if Portal were compromised it could not create containers, run
// commands in them, or read their environment variables (where other
// apps keep secrets). The worst it could do is restart things.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// apiVersion is pinned so a Docker upgrade cannot silently change the
// JSON shapes. 1.44 is the oldest version current Docker engines accept.
const apiVersion = "v1.44"

// Container is what the dashboard shows for one container.
type Container struct {
	ID       string
	Name     string
	Image    string
	State    string // running, exited, restarting, paused, created, dead
	Status   string // human text from Docker: "Up 2 days (healthy)"
	MemBytes uint64 // 0 when not running or unknown
}

// Running reports whether the container is up.
func (c Container) Running() bool { return c.State == "running" }

// Client talks to the Docker API over a unix socket.
type Client struct {
	http *http.Client
}

// New returns a client for the proxy's socket at socketPath.
func New(socketPath string) *Client {
	return &Client{http: &http.Client{
		Transport: &http.Transport{
			// HTTP over a unix socket: the URL's host is ignored and every
			// connection goes to the socket file instead.
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socketPath)
			},
			MaxIdleConns:    4,
			IdleConnTimeout: 30 * time.Second,
		},
	}}
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker/"+apiVersion+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("docker %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// List returns all containers (running and stopped) with memory use for
// the running ones, sorted running-first, then by name.
func (c *Client) List(ctx context.Context) ([]Container, error) {
	var raw []struct {
		ID     string `json:"Id"`
		Names  []string
		Image  string
		State  string
		Status string
	}
	if err := c.do(ctx, http.MethodGet, "/containers/json?all=1", &raw); err != nil {
		return nil, err
	}
	out := make([]Container, len(raw))
	var wg sync.WaitGroup
	for i, r := range raw {
		name := r.ID[:12]
		if len(r.Names) > 0 {
			name = strings.TrimPrefix(r.Names[0], "/")
		}
		out[i] = Container{ID: r.ID, Name: name, Image: r.Image, State: r.State, Status: r.Status}
		if r.State != "running" {
			continue
		}
		// One stats request per running container, all in parallel.
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i].MemBytes = c.memory(ctx, r.ID)
		}()
	}
	wg.Wait()
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Running() != out[b].Running() {
			return out[a].Running()
		}
		return out[a].Name < out[b].Name
	})
	return out, nil
}

// memory returns a container's RAM use the way "docker stats" shows it.
func (c *Client) memory(ctx context.Context, id string) uint64 {
	var st struct {
		MemoryStats struct {
			Usage uint64            `json:"usage"`
			Stats map[string]uint64 `json:"stats"`
		} `json:"memory_stats"`
	}
	// one-shot=true returns a single sample immediately instead of
	// waiting a second to compute CPU deltas we do not need.
	if err := c.do(ctx, http.MethodGet, "/containers/"+id+"/stats?stream=false&one-shot=true", &st); err != nil {
		return 0
	}
	usage := st.MemoryStats.Usage
	// "usage" includes page cache the kernel can drop at any time.
	// Subtract it (cgroup v2: inactive_file; v1: total_inactive_file),
	// exactly as the docker CLI does.
	cache := st.MemoryStats.Stats["inactive_file"]
	if cache == 0 {
		cache = st.MemoryStats.Stats["total_inactive_file"]
	}
	if cache < usage {
		usage -= cache
	}
	return usage
}

var idRe = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

// ValidID reports whether s looks like a container ID. Handlers check
// this before building a URL from user input.
func ValidID(s string) bool { return idRe.MatchString(s) }

// Restart restarts a container (or starts it, if it was stopped), giving
// it 10 seconds to shut down cleanly first.
func (c *Client) Restart(ctx context.Context, id string) error {
	if !ValidID(id) {
		return fmt.Errorf("docker: invalid container id")
	}
	return c.do(ctx, http.MethodPost, "/containers/"+id+"/restart?t=10", nil)
}
