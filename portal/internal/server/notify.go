package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"portal/internal/notify"
)

// ---- Notifications panel ----

// notifItem is one row of the Notifications panel. Everything here ends
// up in HTML through html/template, which escapes it: message text comes
// from other services and must never be trusted as markup.
type notifItem struct {
	Topic   string
	Title   string
	Message string // shortened for the list
	Full    string // complete text, for the tooltip
	Ago     string // "5 min ago"; app.js keeps it current from TS
	At      string // absolute local time, e.g. "Thu 12 Nov 19:04"
	ISO     string // for <time datetime>
	TS      int64  // unix seconds
	Prio    string // "urgent", "high", "", "low" or "quiet": drives the styling
	Tags    string
}

type notifData struct {
	Items []notifItem
	// State: "ok", "off" (no URL/token configured) or "down" (last poll
	// failed). With "down" the last known messages are still shown.
	State string
	Error string
}

// notifRecent is how many messages the log shows before "N older".
const notifRecent = 8

// Recent and Older split the list for the template.
func (d notifData) Recent() []notifItem { return d.Items[:min(len(d.Items), notifRecent)] }
func (d notifData) Older() []notifItem {
	if len(d.Items) <= notifRecent {
		return nil
	}
	return d.Items[notifRecent:]
}

// panelMessageRunes is how much of a message the list shows.
const panelMessageRunes = 280

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimRightFunc(string(r[:n]), func(c rune) bool { return c == ' ' || c == '\n' }) + "…"
}

// ago formats a past moment for people: "just now", "4 min ago", "3 h ago",
// "2 d ago". The same rules are in app.js (relTime) for live updates.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d d ago", int(d.Hours()/24))
}

func prioClass(p int) string {
	switch p {
	case 5:
		return "urgent"
	case 4:
		return "high"
	case 2:
		return "low"
	case 1:
		return "quiet"
	}
	return ""
}

func buildNotif(msgs []notify.Message, st notify.Status, now time.Time) notifData {
	d := notifData{State: "ok"}
	switch {
	case !st.Configured:
		d.State, d.Error = "off", "PORTAL_NTFY_URL or PORTAL_NTFY_TOKEN is not set."
	case !st.OK && !st.CheckedAt.IsZero():
		// The panel already says "ntfy not reachable"; keep only the cause.
		d.State, d.Error = "down", strings.TrimPrefix(st.Err, "ntfy not reachable: ")
	}
	for _, m := range msgs {
		d.Items = append(d.Items, notifItem{
			Topic: m.Topic, Title: m.Title,
			Message: truncateRunes(m.Message, panelMessageRunes), Full: m.Message,
			Ago: ago(m.Time, now), At: m.Time.Local().Format("Mon 2 Jan 15:04"),
			ISO: m.Time.UTC().Format(time.RFC3339), TS: m.Time.Unix(),
			Prio: prioClass(m.Priority), Tags: strings.Join(m.Tags, " "),
		})
	}
	return d
}

func (s *Server) notifView() notifData {
	msgs, st := s.notifyPoll.Snapshot()
	return buildNotif(msgs, st, time.Now())
}

func (s *Server) publishNotify() {
	s.hub.Publish("notify", strings.TrimSpace(s.fragment("notify", s.notifView())))
}

// refreshNotify polls ntfy and pushes the panel to open dashboards if
// anything changed.
func (s *Server) refreshNotify(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	changed, err := s.notifyPoll.Refresh(ctx)
	if err != nil && changed {
		log.Printf("notifications: %v", err) // logged once per outage, not every 15s
	}
	if changed {
		s.publishNotify()
	}
}

// ---- POST /api/notify ----

// handleNotify lets a service send a notification through Portal instead
// of talking to ntfy directly (see NOTIFY.md for examples):
//
//	POST http://exp.lab/api/notify
//	Authorization: Bearer tk_...        (the service's own ntfy token)
//	{"topic":"backups","title":"...","message":"...","priority":"urgent","tags":["warning"]}
//
// It is for machines, so there is no session and no Origin check. It is
// still not a way around anything: Portal forwards the CALLER'S token, and
// ntfy alone decides whether that token may publish to that topic. (A
// browser page on another site cannot abuse this either: it cannot add an
// Authorization header to a cross-site request without a CORS preflight,
// which Portal never approves.)
func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	reply := func(code int, result string, body map[string]any) {
		s.metrics.Notify(result)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(body)
	}
	fail := func(code int, result, msg string) {
		reply(code, result, map[string]any{"ok": false, "error": msg})
	}

	// Rate limit first, before any work, keyed like the login limiter.
	ip := clientIP(r)
	if ok, retry := s.notifyLimit.Allow(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
		fail(http.StatusTooManyRequests, "rate_limited", "too many requests: at most 60 per minute")
		return
	}
	token, ok := notify.BearerToken(r.Header.Get("Authorization"))
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ntfy"`)
		fail(http.StatusUnauthorized, "unauthorized", "send your ntfy token as 'Authorization: Bearer tk_...'")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		fail(http.StatusUnsupportedMediaType, "invalid", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, notify.MaxBodyBytes)
	msg, err := notify.DecodeRequest(r.Body)
	if err != nil {
		fail(http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if !s.notifyPub.Configured() {
		fail(http.StatusServiceUnavailable, "upstream_error", "ntfy is not configured in Portal (PORTAL_NTFY_URL)")
		return
	}

	res, err := s.notifyPub.Publish(r.Context(), token, ip, msg)
	switch {
	case err != nil:
		fail(http.StatusBadGateway, "upstream_error", "ntfy not reachable")
	case res.Status == http.StatusOK:
		reply(http.StatusOK, "ok", map[string]any{"ok": true, "id": res.ID})
		// Show it on the dashboard now rather than at the next poll.
		go s.refreshNotify(context.WithoutCancel(r.Context()))
	case res.Status == http.StatusUnauthorized:
		fail(http.StatusUnauthorized, "unauthorized", "ntfy does not know this token")
	case res.Status == http.StatusForbidden:
		fail(http.StatusForbidden, "forbidden", "this token may not publish to topic '"+msg.Topic+"'")
	case res.Status == http.StatusTooManyRequests:
		fail(http.StatusTooManyRequests, "rate_limited", "ntfy rate limit reached, try again later")
	case res.Status >= 400 && res.Status < 500:
		fail(res.Status, "invalid", "ntfy refused the message: "+res.Error)
	default:
		fail(http.StatusBadGateway, "upstream_error", fmt.Sprintf("ntfy answered HTTP %d", res.Status))
	}
}
