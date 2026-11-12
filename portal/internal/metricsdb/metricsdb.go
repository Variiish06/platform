// Package metricsdb is a minimal client for the Prometheus HTTP query API,
// which VictoriaMetrics implements. Portal uses it for the /graphs page and
// the uptime bars on the app tiles.
//
// Two calls are all a dashboard needs:
//
//   - Query      GET /api/v1/query?query=...&time=T
//     evaluates an expression at ONE moment. The answer is a "vector":
//     one value per series, e.g. the current CPU % of every container.
//
//   - QueryRange GET /api/v1/query_range?query=...&start=A&end=B&step=S
//     evaluates the same expression at A, A+S, A+2S, ... up to B. The
//     answer is a "matrix": for each series, a list of (timestamp, value)
//     points. That is a line on a chart. Note that the database does not
//     return the raw scraped samples: at every step it computes the
//     expression again, looking back from that instant (functions such as
//     rate(x[5m]) or avg_over_time(x[1d]) say how far back). So the step
//     decides the resolution of the chart, not the scrape interval.
//
// The wire format, abbreviated:
//
//	{"status":"success","data":{"resultType":"matrix","result":[
//	   {"metric":{"app":"focal"},"values":[[1731400000,"1"],[1731400060,"0.5"]]}]}}
//
// Values are strings so that NaN and +Inf survive JSON; ParseFloat
// understands both.
package metricsdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MaxResponseBytes caps how much of a response is read. A 30-day range
// with 360 points for 20 series is ~250 KB; 8 MB means a bad query
// (thousands of series) fails cleanly instead of eating Portal's memory.
const MaxResponseBytes = 8 << 20

// ErrTooLarge is returned when a response exceeds MaxResponseBytes.
var ErrTooLarge = errors.New("metricsdb: response too large")

// Point is one (time, value) pair.
type Point struct {
	T time.Time
	V float64
}

// Series is one time series: its labels and its points (matrix) or its
// single point (vector).
type Series struct {
	Labels map[string]string
	Points []Point
}

// Client talks to one VictoriaMetrics/Prometheus server.
type Client struct {
	base string
	http *http.Client
}

// New returns a client for baseURL (e.g. "http://127.0.0.1:8428"), or nil
// when baseURL is empty, which callers treat as "not configured".
func New(baseURL string) *Client {
	if baseURL == "" {
		return nil
	}
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			// A ceiling for every call. Callers pass shorter context
			// deadlines; this only catches a forgotten one.
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
				ResponseHeaderTimeout: 10 * time.Second,
				MaxIdleConns:          4,
				MaxIdleConnsPerHost:   4,
				IdleConnTimeout:       60 * time.Second,
			},
		},
	}
}

// Configured reports whether a URL was given.
func (c *Client) Configured() bool { return c != nil }

// Health asks VictoriaMetrics' /health endpoint (answers "OK").
func (c *Client) Health(ctx context.Context) error {
	if c == nil {
		return errors.New("metricsdb: not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("metricsdb: health: HTTP %d", resp.StatusCode)
	}
	return nil
}

// Query evaluates expr at time t (an "instant query").
func (c *Client) Query(ctx context.Context, expr string, t time.Time) ([]Series, error) {
	v := url.Values{"query": {expr}, "time": {formatTime(t)}}
	return c.get(ctx, "/api/v1/query", v)
}

// QueryRange evaluates expr at start, start+step, ... end.
//
// nocache=1 is a VictoriaMetrics extension (Prometheus ignores unknown
// parameters). Without it VM may round start/end to a multiple of step to
// improve its cache hit rate; for 1-day steps that would move "midnight
// IST" to "midnight UTC". The response cache is not worth that here:
// these queries run every few minutes at most.
func (c *Client) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]Series, error) {
	if step <= 0 {
		return nil, errors.New("metricsdb: step must be positive")
	}
	v := url.Values{
		"query": {expr}, "start": {formatTime(start)}, "end": {formatTime(end)},
		"step": {strconv.FormatFloat(step.Seconds(), 'f', -1, 64) + "s"}, "nocache": {"1"},
	}
	return c.get(ctx, "/api/v1/query_range", v)
}

func formatTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', -1, 64)
}

// apiResponse is the envelope of every query response.
type apiResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string    `json:"metric"`
			Value  [2]json.RawMessage   `json:"value"`  // vector
			Values [][2]json.RawMessage `json:"values"` // matrix
		} `json:"result"`
	} `json:"data"`
}

func (c *Client) get(ctx context.Context, path string, params url.Values) ([]Series, error) {
	if c == nil {
		return nil, errors.New("metricsdb: not configured")
	}
	// POST with a form body would hide long queries from access logs, but
	// GET keeps it debuggable with curl; our queries are short.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Read one byte more than allowed: if we get it, the body was too big.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxResponseBytes {
		return nil, ErrTooLarge
	}
	var r apiResponse
	if err := json.Unmarshal(body, &r); err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("metricsdb: HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("metricsdb: bad response: %w", err)
	}
	if r.Status != "success" {
		msg := r.Error
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
		return nil, fmt.Errorf("metricsdb: %s: %s", r.ErrorType, msg)
	}
	switch r.Data.ResultType {
	case "vector", "matrix":
	default:
		return nil, fmt.Errorf("metricsdb: unsupported result type %q", r.Data.ResultType)
	}
	out := make([]Series, 0, len(r.Data.Result))
	for _, res := range r.Data.Result {
		s := Series{Labels: res.Metric}
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		pairs := res.Values
		if r.Data.ResultType == "vector" {
			pairs = [][2]json.RawMessage{res.Value}
		}
		for _, p := range pairs {
			pt, err := parsePoint(p)
			if err != nil {
				return nil, err
			}
			s.Points = append(s.Points, pt)
		}
		out = append(out, s)
	}
	return out, nil
}

// parsePoint decodes [1731400000.123, "0.5"].
func parsePoint(p [2]json.RawMessage) (Point, error) {
	var ts float64
	var vs string
	if err := json.Unmarshal(p[0], &ts); err != nil {
		return Point{}, fmt.Errorf("metricsdb: bad timestamp: %w", err)
	}
	if err := json.Unmarshal(p[1], &vs); err != nil {
		return Point{}, fmt.Errorf("metricsdb: bad value: %w", err)
	}
	v, err := strconv.ParseFloat(vs, 64)
	if err != nil {
		return Point{}, fmt.Errorf("metricsdb: bad value %q", vs)
	}
	sec, frac := math.Modf(ts)
	return Point{T: time.Unix(int64(sec), int64(math.Round(frac*1e3))*int64(time.Millisecond)), V: v}, nil
}
