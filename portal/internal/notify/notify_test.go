package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	portalToken = "tk_portalportalportalportalporta" // Portal's own (read-only) token
	callerToken = "tk_callercallercallercallercalle" // a service's write token
)

func init() {
	for _, tok := range []string{portalToken, callerToken} {
		if !tokenRe.MatchString(tok) {
			panic("test token has the wrong shape: " + tok)
		}
	}
}

func msg(id string, t int64) Message {
	return Message{ID: id, Time: time.Unix(t, 0), Topic: "general", Priority: 3}
}

func ids(list []Message) string {
	var s []string
	for _, m := range list {
		s = append(s, m.ID)
	}
	return strings.Join(s, ",")
}

func TestMergeOrderDedupTruncate(t *testing.T) {
	cur := []Message{msg("c", 30), msg("b", 20), msg("a", 10)}
	fresh := []Message{msg("d", 40), msg("c", 30), msg("e", 25)}
	got := Merge(cur, fresh, 4)
	if ids(got) != "d,c,e,b" {
		t.Fatalf("Merge = %s, want d,c,e,b", ids(got))
	}

	// 25 messages in, newest 20 out, newest first.
	var many []Message
	for i := 1; i <= 25; i++ {
		many = append(many, msg(fmt.Sprintf("m%02d", i), int64(i)))
	}
	got = Merge(nil, many, MaxMessages)
	if len(got) != MaxMessages || got[0].ID != "m25" || got[19].ID != "m06" {
		t.Fatalf("truncate: len=%d first=%s last=%s", len(got), got[0].ID, got[len(got)-1].ID)
	}

	// Same second: stable order by ID.
	got = Merge(nil, []Message{msg("x1", 5), msg("x3", 5), msg("x2", 5)}, 10)
	if ids(got) != "x3,x2,x1" {
		t.Fatalf("tie order = %s", ids(got))
	}
}

func TestParseStream(t *testing.T) {
	in := `{"id":"o1","time":1,"event":"open","topic":"system"}
{"id":"m1","time":100,"event":"message","topic":"system","title":"Disk","message":"85% full","priority":5,"tags":["warning"]}
{"id":"m2","time":101,"event":"message","topic":"general","message":"hi"}
{"id":"m3","time":102,"event":"message","topic":"elsewhere","message":"not ours"}
{"id":"k1","time":103,"event":"keepalive","topic":"system"}
`
	got, err := parseStream(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if ids(got) != "m1,m2" {
		t.Fatalf("parsed %s, want m1,m2", ids(got))
	}
	if got[0].Priority != 5 || got[0].Title != "Disk" || got[0].Tags[0] != "warning" {
		t.Fatalf("fields lost: %+v", got[0])
	}
	if got[1].Priority != 3 {
		t.Fatalf("missing priority should default to 3, got %d", got[1].Priority)
	}
	if _, err := parseStream(strings.NewReader("not json\n")); err == nil {
		t.Fatal("garbage accepted")
	}
}

// fakeNtfy serves the poll endpoint from a slice of lines and records
// the requests it saw.
type fakeNtfy struct {
	mu    sync.Mutex
	lines []string
	reqs  []*http.Request
	code  int
}

func (f *fakeNtfy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r.Clone(context.Background()))
	if f.code != 0 {
		w.WriteHeader(f.code)
		return
	}
	since := r.URL.Query().Get("since")
	for _, l := range f.lines {
		var m wireMessage
		json.Unmarshal([]byte(l), &m)
		if since != "all" && m.ID <= since { // IDs in the test sort by time
			continue
		}
		io.WriteString(w, l+"\n")
	}
}

func line(id string, t int64, topic string) string {
	return fmt.Sprintf(`{"id":%q,"time":%d,"event":"message","topic":%q,"message":"msg %s"}`, id, t, topic, id)
}

func TestPollerRefresh(t *testing.T) {
	f := &fakeNtfy{lines: []string{line("a01", 1, "system"), line("a02", 2, "backups")}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	p := NewPoller(srv.URL, portalToken)

	changed, err := p.Refresh(context.Background())
	if err != nil || !changed {
		t.Fatalf("first refresh: changed=%v err=%v", changed, err)
	}
	got, st := p.Snapshot()
	if ids(got) != "a02,a01" || !st.OK || !st.Configured {
		t.Fatalf("snapshot %s %+v", ids(got), st)
	}
	r := f.reqs[0]
	if r.URL.Path != "/system,backups,focal,ritual,dalal,general/json" {
		t.Errorf("path %q", r.URL.Path)
	}
	if r.URL.Query().Get("poll") != "1" || r.URL.Query().Get("since") != "all" {
		t.Errorf("query %q", r.URL.RawQuery)
	}
	if r.Header.Get("Authorization") != "Bearer "+portalToken {
		t.Errorf("poll not authenticated with Portal's token")
	}

	// Nothing new: no change, and it asked only for messages after a02.
	changed, _ = p.Refresh(context.Background())
	if changed || f.reqs[1].URL.Query().Get("since") != "a02" {
		t.Fatalf("second refresh: changed=%v since=%q", changed, f.reqs[1].URL.Query().Get("since"))
	}

	// 25 new messages: the list stays at 20, newest first.
	for i := 3; i <= 27; i++ {
		f.lines = append(f.lines, line(fmt.Sprintf("a%02d", i), int64(i), "general"))
	}
	p.Refresh(context.Background())
	got, _ = p.Snapshot()
	if len(got) != MaxMessages || got[0].ID != "a27" || got[19].ID != "a08" {
		t.Fatalf("after flood: len=%d first=%s last=%s", len(got), got[0].ID, got[len(got)-1].ID)
	}

	// ntfy rejects the token: status shows it, messages are kept.
	f.code = http.StatusForbidden
	changed, err = p.Refresh(context.Background())
	if err == nil || !changed {
		t.Fatalf("403 not reported: changed=%v err=%v", changed, err)
	}
	got, st = p.Snapshot()
	if st.OK || !strings.Contains(st.Err, "NTFY_TOKEN_PORTAL") || len(got) != MaxMessages {
		t.Fatalf("after 403: %+v len=%d", st, len(got))
	}
	if strings.Contains(st.Err, portalToken) {
		t.Fatal("error text contains the token")
	}
}

func TestPollerUnconfiguredAndUnreachable(t *testing.T) {
	p := NewPoller("http://127.0.0.1:2586", "")
	if changed, err := p.Refresh(context.Background()); changed || err != nil {
		t.Fatalf("unconfigured poller did something: %v %v", changed, err)
	}
	if _, st := p.Snapshot(); st.Configured {
		t.Fatal("empty token counted as configured")
	}

	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens there any more
	p = NewPoller(url, portalToken)
	if _, err := p.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("unreachable: err=%v", err)
	}
}

func TestParsePriority(t *testing.T) {
	cases := map[string]int{
		``: 3, `null`: 3, `"urgent"`: 5, `"URGENT"`: 5, `"normal"`: 3, `"quiet"`: 1,
		`1`: 1, `3`: 3, `5`: 5, `"4"`: 4, `"2"`: 2,
	}
	for in, want := range cases {
		got, err := ParsePriority(json.RawMessage(in))
		if err != nil || got != want {
			t.Errorf("ParsePriority(%s) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{`0`, `6`, `-1`, `2.5`, `"high"`, `"max"`, `true`, `[]`, `{}`} {
		if _, err := ParsePriority(json.RawMessage(bad)); err == nil {
			t.Errorf("ParsePriority(%s) accepted", bad)
		}
	}
}

func TestDecodeRequest(t *testing.T) {
	ok := `{"topic":"backups","title":"Backup","message":"done","priority":"quiet","tags":["floppy_disk"],"click":"http://exp.lab/"}`
	p, err := DecodeRequest(strings.NewReader(ok))
	if err != nil {
		t.Fatal(err)
	}
	if p.Topic != "backups" || p.Priority != 1 || p.Title != "Backup" || p.Tags[0] != "floppy_disk" || p.Click != "http://exp.lab/" {
		t.Fatalf("decoded %+v", p)
	}
	if p, _ := DecodeRequest(strings.NewReader(`{"topic":"general","message":"x"}`)); p.Priority != 3 {
		t.Fatalf("default priority %d", p.Priority)
	}

	bad := map[string]string{
		"unknown topic":   `{"topic":"secret","message":"x"}`,
		"no topic":        `{"message":"x"}`,
		"no message":      `{"topic":"general"}`,
		"blank message":   `{"topic":"general","message":"   "}`,
		"long message":    `{"topic":"general","message":"` + strings.Repeat("a", MaxMessageLen+1) + `"}`,
		"bad priority":    `{"topic":"general","message":"x","priority":9}`,
		"typo field":      `{"topic":"general","message":"x","priorty":5}`,
		"tag with space":  `{"topic":"general","message":"x","tags":["two words"]}`,
		"too many tags":   `{"topic":"general","message":"x","tags":["a","b","c","d","e","f","g","h","i","j","k"]}`,
		"js click":        `{"topic":"general","message":"x","click":"javascript:alert(1)"}`,
		"title newline":   `{"topic":"general","message":"x","title":"a\nb"}`,
		"not an object":   `["general","x"]`,
		"two objects":     `{"topic":"general","message":"x"}{"topic":"general","message":"y"}`,
		"tags not a list": `{"topic":"general","message":"x","tags":"warning"}`,
	}
	for name, body := range bad {
		if _, err := DecodeRequest(strings.NewReader(body)); err == nil {
			t.Errorf("%s: accepted %s", name, body)
		}
	}
}

func TestBearerToken(t *testing.T) {
	if tok, ok := BearerToken("Bearer " + callerToken); !ok || tok != callerToken {
		t.Fatal("valid header rejected")
	}
	if _, ok := BearerToken("bearer " + callerToken); !ok {
		t.Fatal("scheme should be case-insensitive")
	}
	for _, h := range []string{"", "Bearer", "Bearer ", "Basic " + callerToken, "Bearer tk_short",
		"Bearer " + callerToken + "x", "Bearer pk_callercallercallercallercal", "Token " + callerToken} {
		if _, ok := BearerToken(h); ok {
			t.Errorf("BearerToken(%q) accepted", h)
		}
	}
}

func TestPublishUsesCallersToken(t *testing.T) {
	var gotAuth, gotXFF, gotCT, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotXFF, gotCT, gotPath = r.Header.Get("Authorization"), r.Header.Get("X-Forwarded-For"), r.Header.Get("Content-Type"), r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		io.WriteString(w, `{"id":"abc123","time":1,"event":"message","topic":"general","message":"hi"}`)
	}))
	defer srv.Close()

	// A poller with Portal's token exists in the same process: the
	// publisher must never borrow it.
	_ = NewPoller(srv.URL, portalToken)
	pub := NewPublisher(srv.URL)
	res, err := pub.Publish(context.Background(), callerToken, "100.64.0.9",
		Publish{Topic: "general", Title: "T", Message: "hi", Priority: 5, Tags: []string{"tada"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 || res.ID != "abc123" {
		t.Fatalf("result %+v", res)
	}
	if gotAuth != "Bearer "+callerToken {
		t.Fatalf("Authorization sent = %q, want the caller's token", gotAuth)
	}
	if strings.Contains(gotAuth, portalToken) {
		t.Fatal("Portal's own token was sent on publish")
	}
	if gotXFF != "100.64.0.9" || gotCT != "application/json" || gotPath != "/" {
		t.Fatalf("xff=%q ct=%q path=%q", gotXFF, gotCT, gotPath)
	}
	if gotBody["topic"] != "general" || gotBody["priority"] != float64(5) || gotBody["title"] != "T" {
		t.Fatalf("body %v", gotBody)
	}
}

func TestPublishPassesNtfyErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"code":40301,"http":403,"error":"forbidden"}`)
	}))
	defer srv.Close()
	res, err := NewPublisher(srv.URL).Publish(context.Background(), callerToken, "", Publish{Topic: "system", Message: "x", Priority: 3})
	if err != nil || res.Status != 403 || res.Error != "forbidden" {
		t.Fatalf("res=%+v err=%v", res, err)
	}

	srv.Close()
	if _, err := NewPublisher(srv.URL).Publish(context.Background(), callerToken, "", Publish{Topic: "system", Message: "x"}); err == nil {
		t.Fatal("unreachable ntfy not reported")
	}
}

func TestRateLimiter(t *testing.T) {
	l := NewRateLimiter(3, time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d refused", i+1)
		}
	}
	ok, retry := l.Allow("a")
	if ok || retry != time.Minute {
		t.Fatalf("4th request: ok=%v retry=%v", ok, retry)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("other client affected")
	}
	now = now.Add(time.Minute)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("not reset after the window")
	}
	now = now.Add(2 * time.Minute)
	if n := l.Cleanup(); n != 0 {
		t.Fatalf("cleanup left %d", n)
	}
}

// Bare CRs in a message become "\n" (the browser's EventSource would read
// them as line ends); titles lose every line break.
func TestParseStreamNormalizesCR(t *testing.T) {
	in := `{"id":"a","time":1,"event":"message","topic":"general","title":"t\r\nevent: x","message":"a\r\revent: backup\rdata: x\r\nb"}` + "\n"
	got, err := parseStream(strings.NewReader(in))
	if err != nil || len(got) != 1 {
		t.Fatalf("parseStream: %v %v", got, err)
	}
	if got[0].Message != "a\n\nevent: backup\ndata: x\nb" {
		t.Fatalf("message = %q", got[0].Message)
	}
	if got[0].Title != "t event: x" {
		t.Fatalf("title = %q", got[0].Title)
	}
}
