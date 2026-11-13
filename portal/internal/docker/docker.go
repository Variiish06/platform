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
	"time"
)

// apiVersion is pinned so a Docker upgrade cannot silently change the
// JSON shapes. 1.44 is the oldest version current Docker engines accept.
const apiVersion = "v1.44"

// Container is what the dashboard shows for one container.
type Container struct {
	ID     string
	Name   string // without Docker's leading slash
	Image  string
	State  string // running, exited, restarting, paused, created, dead
	Status string // human text from Docker: "Up 2 days (healthy)"
	// Project is the compose project (label com.docker.compose.project),
	// "" for containers started without compose.
	Project string

	// Filled in by the stats collector (see stats.go), running containers only.
	MemBytes   uint64  // 0 when not running or unknown
	MemOK      bool    // MemBytes is a real reading
	CPUPercent float64 // 100 = one full core, like "docker stats"
	CPUOK      bool    // CPUPercent is a real reading (needs two samples)
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
			// The stats collector runs up to statsWorkers requests at once;
			// keep that many connections around between rounds.
			MaxIdleConns:        statsWorkers,
			MaxIdleConnsPerHost: statsWorkers,
			IdleConnTimeout:     30 * time.Second,
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
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	// A stats document is ~3 KB and a list of 20 containers ~30 KB; 4 MB
	// is a generous ceiling that still stops a runaway response.
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// List returns all containers (running and stopped), sorted running-first,
// then by name. It makes exactly one API call; resource usage comes from
// Stats, which the caller runs per container (see the collector in the
// server package).
func (c *Client) List(ctx context.Context) ([]Container, error) {
	var raw []struct {
		ID     string `json:"Id"`
		Names  []string
		Image  string
		State  string
		Status string
		Labels map[string]string
	}
	if err := c.do(ctx, http.MethodGet, "/containers/json?all=1", &raw); err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(raw))
	for _, r := range raw {
		if !ValidID(r.ID) {
			continue // never put something we cannot validate into a URL later
		}
		name := r.ID[:12]
		if len(r.Names) > 0 {
			name = strings.TrimPrefix(r.Names[0], "/")
		}
		out = append(out, Container{ID: r.ID, Name: name, Image: r.Image, State: r.State, Status: r.Status,
			Project: r.Labels["com.docker.compose.project"]})
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Running() != out[b].Running() {
			return out[a].Running()
		}
		return out[a].Name < out[b].Name
	})
	return out, nil
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
