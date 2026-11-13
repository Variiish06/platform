// Package notify talks to ntfy, the notification hub (see NOTIFY.md).
//
// ntfy is publish/subscribe: services PUBLISH short messages to named
// TOPICS and anyone allowed to read a topic can SUBSCRIBE to it. Portal does
// both, in two very different ways:
//
//   - Reading: a background Poller asks ntfy every ~15 s for new messages
//     on the six topics, using Portal's own READ-ONLY token, and keeps the
//     newest 20 for the dashboard's Notifications panel.
//   - Writing: POST /api/notify (in package server) checks a caller's
//     request and passes it to Publish together with the CALLER'S token.
//     Portal holds no write token at all; ntfy decides who may publish
//     where.
package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Topics is the fixed list of topics on this server. It is the one place
// they are named in Go: the poller reads exactly these, and /api/notify
// refuses any other topic.
var Topics = []string{"system", "backups", "focal", "ritual", "dalal", "general"}

// KnownTopic reports whether t is one of Topics.
func KnownTopic(t string) bool {
	for _, k := range Topics {
		if k == t {
			return true
		}
	}
	return false
}

// MaxMessages is how many recent messages the poller keeps.
const MaxMessages = 20

// Message is one notification as the dashboard needs it.
type Message struct {
	ID       string
	Time     time.Time
	Topic    string
	Title    string
	Message  string
	Priority int // 1..5, 3 when the publisher did not say
	Tags     []string
}

// wireMessage is one line of ntfy's JSON stream ("/<topics>/json").
// See https://docs.ntfy.sh/subscribe/api/#json-message-format
type wireMessage struct {
	ID       string   `json:"id"`
	Time     int64    `json:"time"`
	Event    string   `json:"event"` // "message", "open", "keepalive", ...
	Topic    string   `json:"topic"`
	Title    string   `json:"title"`
	Message  string   `json:"message"`
	Priority int      `json:"priority"`
	Tags     []string `json:"tags"`
}

// Status is what the panel shows about the connection to ntfy.
type Status struct {
	Configured bool      // a URL and a token are set
	OK         bool      // the last poll succeeded
	Err        string    // why the last poll failed (no secrets in it)
	CheckedAt  time.Time // when the last poll finished
}

// Poller keeps the newest MaxMessages messages across Topics.
type Poller struct {
	base   string // e.g. "http://127.0.0.1:2586", no trailing slash
	token  string // read-only token (svc-portal)
	client *http.Client

	mu     sync.RWMutex
	msgs   []Message // newest first, at most MaxMessages
	lastID string    // newest message ID seen: the next poll asks only for newer ones
	status Status
}

// NewPoller returns a poller for the ntfy server at base. With an empty
// base or token it is "not configured" and Refresh does nothing.
func NewPoller(base, token string) *Poller {
	return &Poller{
		base:   strings.TrimRight(base, "/"),
		token:  token,
		client: &http.Client{Timeout: 10 * time.Second},
		status: Status{Configured: base != "" && token != ""},
	}
}

// Snapshot returns a copy of the current messages and connection status.
func (p *Poller) Snapshot() ([]Message, Status) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]Message(nil), p.msgs...), p.status
}

// Refresh polls ntfy once and merges new messages in. It reports whether
// the list changed, so callers only push an update when there is one.
//
// The request is GET /system,backups,.../json?poll=1&since=<x>:
//   - "poll=1" makes ntfy answer with what it has and close the response,
//     instead of keeping the connection open (streaming).
//   - "since" is "all" the first time (everything still in the 7-day
//     cache) and afterwards the newest message ID we have, so each poll
//     only transfers what is new.
func (p *Poller) Refresh(ctx context.Context) (bool, error) {
	if !p.status.Configured {
		return false, nil
	}
	p.mu.RLock()
	since := p.lastID
	p.mu.RUnlock()
	if since == "" {
		since = "all"
	}
	fresh, err := p.fetch(ctx, since)

	p.mu.Lock()
	defer p.mu.Unlock()
	wasOK := p.status.OK
	p.status.CheckedAt = time.Now()
	if err != nil {
		p.status.OK, p.status.Err = false, err.Error()
		return wasOK, err // changed if it just went from OK to failing
	}
	p.status.OK, p.status.Err = true, ""
	merged := Merge(p.msgs, fresh, MaxMessages)
	changed := !wasOK || !sameIDs(merged, p.msgs)
	p.msgs = merged
	if len(merged) > 0 {
		p.lastID = merged[0].ID
	}
	return changed, nil
}

func (p *Poller) fetch(ctx context.Context, since string) ([]Message, error) {
	u := p.base + "/" + strings.Join(Topics, ",") + "/json?poll=1&since=" + url.QueryEscape(since)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := p.client.Do(req)
	if err != nil {
		// The error text names the URL (no token in it) and the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("ntfy not reachable: %v", ue.Err)
		}
		return nil, fmt.Errorf("ntfy not reachable: %v", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("ntfy refused Portal's token (HTTP %d): check NTFY_TOKEN_PORTAL", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("ntfy answered HTTP %d", resp.StatusCode)
	}
	return parseStream(io.LimitReader(resp.Body, 8<<20))
}

// parseStream reads ntfy's newline-delimited JSON and keeps the
// "message" events (a poll can also contain "open"/"keepalive" lines).
func parseStream(r io.Reader) ([]Message, error) {
	var out []Message
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var w wireMessage
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, fmt.Errorf("ntfy sent something that is not JSON: %w", err)
		}
		if w.Event != "message" || !KnownTopic(w.Topic) {
			continue
		}
		prio := w.Priority
		if prio < 1 || prio > 5 {
			prio = 3 // ntfy omits the field for the default priority
		}
		out = append(out, Message{ID: w.ID, Time: time.Unix(w.Time, 0), Topic: w.Topic,
			Title: oneLine(w.Title), Message: normalizeNewlines(w.Message), Priority: prio, Tags: w.Tags})
	}
	return out, sc.Err()
}

// Merge combines the current list with newly fetched messages: no
// duplicates (by ID), newest first, at most limit entries. Messages with
// the same second keep a stable order by ID so the panel does not flicker.
func Merge(current, fresh []Message, limit int) []Message {
	seen := make(map[string]bool, len(current)+len(fresh))
	all := make([]Message, 0, len(current)+len(fresh))
	for _, list := range [][]Message{fresh, current} {
		for _, m := range list {
			if m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			all = append(all, m)
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].Time.Equal(all[j].Time) {
			return all[i].Time.After(all[j].Time)
		}
		return all[i].ID > all[j].ID
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

func sameIDs(a, b []Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

// normalizeNewlines turns "\r\n" and lone "\r" into "\n". Messages come
// from any token holder; a bare "\r" means nothing on the dashboard but is
// a line break to the browser's EventSource (see server/hub.go formatEvent,
// which also guards against it: defence in depth).
func normalizeNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// oneLine replaces any line break in a title with a space.
func oneLine(s string) string {
	return strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s)
}
