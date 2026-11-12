package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits for POST /api/notify. ntfy's own message limit is 4096 bytes;
// anything longer would be turned into an attachment, which is disabled.
const (
	MaxBodyBytes   = 8 << 10
	MaxMessageLen  = 4096
	MaxTitleLen    = 250
	MaxTags        = 10
	MaxTagLen      = 32
	MaxClickURLLen = 512
)

// tokenRe is ntfy's access-token format: "tk_" + 29 characters.
var tokenRe = regexp.MustCompile(`^tk_[-_A-Za-z0-9]{29}$`)

// tagRe: tags are short words; ntfy shows known ones as emoji
// ("warning", "white_check_mark") and the rest as text.
var tagRe = regexp.MustCompile(`^[A-Za-z0-9_+:-]+$`)

// BearerToken extracts a well-formed ntfy token from an Authorization
// header ("Bearer tk_..."). ok is false if it is missing or malformed.
func BearerToken(header string) (token string, ok bool) {
	scheme, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	if !tokenRe.MatchString(value) {
		return "", false
	}
	return value, true
}

// Request is the JSON body of POST /api/notify.
type Request struct {
	Topic    string          `json:"topic"`
	Title    string          `json:"title"`
	Message  string          `json:"message"`
	Priority json.RawMessage `json:"priority"` // "urgent"|"normal"|"quiet" or 1..5
	Tags     []string        `json:"tags"`
	Click    string          `json:"click"`
}

// Publish is a validated message, in ntfy's JSON publish format
// (https://docs.ntfy.sh/publish/#publish-as-json).
type Publish struct {
	Topic    string   `json:"topic"`
	Title    string   `json:"title,omitempty"`
	Message  string   `json:"message"`
	Priority int      `json:"priority"`
	Tags     []string `json:"tags,omitempty"`
	Click    string   `json:"click,omitempty"`
}

// ParsePriority accepts the names "urgent" (5), "normal" (3), "quiet" (1),
// or a number 1..5 written as a JSON number or string. Missing or null
// means normal.
func ParsePriority(raw json.RawMessage) (int, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 3, nil
	}
	if strings.HasPrefix(s, `"`) {
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, errors.New("priority must be urgent, normal, quiet or 1-5")
		}
		s = strings.ToLower(strings.TrimSpace(s))
	}
	switch s {
	case "urgent":
		return 5, nil
	case "normal", "":
		return 3, nil
	case "quiet":
		return 1, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 5 {
		return 0, errors.New("priority must be urgent, normal, quiet or 1-5")
	}
	return n, nil
}

// DecodeRequest reads and validates a request body. Unknown fields are an
// error, so a typo like "priorty" is reported instead of silently ignored.
func DecodeRequest(body io.Reader) (Publish, error) {
	var req Request
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return Publish{}, fmt.Errorf("body is larger than %d bytes", MaxBodyBytes)
		}
		return Publish{}, fmt.Errorf("body must be a JSON object: %v", err)
	}
	if dec.More() {
		return Publish{}, errors.New("body must be a single JSON object")
	}
	return req.Validate()
}

// Validate checks every field and returns the message to send.
func (r Request) Validate() (Publish, error) {
	p := Publish{Topic: strings.TrimSpace(r.Topic), Title: strings.TrimSpace(r.Title), Message: r.Message}
	if !KnownTopic(p.Topic) {
		return Publish{}, fmt.Errorf("topic must be one of: %s", strings.Join(Topics, ", "))
	}
	if strings.TrimSpace(p.Message) == "" {
		return Publish{}, errors.New("message is required")
	}
	if len(p.Message) > MaxMessageLen {
		return Publish{}, fmt.Errorf("message is longer than %d bytes", MaxMessageLen)
	}
	if utf8.RuneCountInString(p.Title) > MaxTitleLen || strings.ContainsAny(p.Title, "\r\n") {
		return Publish{}, fmt.Errorf("title must be one line of at most %d characters", MaxTitleLen)
	}
	if !utf8.ValidString(p.Message) || !utf8.ValidString(p.Title) {
		return Publish{}, errors.New("title and message must be UTF-8 text")
	}
	prio, err := ParsePriority(r.Priority)
	if err != nil {
		return Publish{}, err
	}
	p.Priority = prio
	if len(r.Tags) > MaxTags {
		return Publish{}, fmt.Errorf("at most %d tags", MaxTags)
	}
	for _, t := range r.Tags {
		if len(t) == 0 || len(t) > MaxTagLen || !tagRe.MatchString(t) {
			return Publish{}, fmt.Errorf("tag %q: tags are words of at most %d letters, digits, _ + : -", t, MaxTagLen)
		}
	}
	p.Tags = r.Tags
	if r.Click != "" {
		u, err := url.Parse(r.Click)
		if err != nil || len(r.Click) > MaxClickURLLen || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Publish{}, fmt.Errorf("click must be an http(s) URL of at most %d characters", MaxClickURLLen)
		}
		p.Click = r.Click
	}
	return p, nil
}

// PublishResult is ntfy's answer to a publish.
type PublishResult struct {
	Status int    // ntfy's HTTP status, or 0 if ntfy could not be reached
	ID     string // message ID on success
	Error  string // ntfy's error text otherwise
}

// Publisher sends messages to ntfy. It deliberately has no token of its
// own: every call carries the token of whoever asked Portal to publish.
type Publisher struct {
	base   string
	client *http.Client
}

// NewPublisher returns a publisher for the ntfy server at base.
func NewPublisher(base string) *Publisher {
	return &Publisher{base: strings.TrimRight(base, "/"), client: &http.Client{Timeout: 10 * time.Second}}
}

// Configured reports whether an ntfy URL is set.
func (p *Publisher) Configured() bool { return p.base != "" }

// Publish posts msg to ntfy's root URL as JSON, authenticated with token
// (the caller's). clientIP is passed as X-Forwarded-For, so ntfy's own
// rate limiting and logs see the real caller rather than Portal.
func (p *Publisher) Publish(ctx context.Context, token, clientIP string, msg Publish) (PublishResult, error) {
	body, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/", bytes.NewReader(body))
	if err != nil {
		return PublishResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if clientIP != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return PublishResult{}, errors.New("ntfy not reachable")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	res := PublishResult{Status: resp.StatusCode}
	// ntfy answers with JSON both ways: the stored message on success,
	// {"code":40301,"http":403,"error":"forbidden",...} on failure.
	var parsed struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	json.Unmarshal(raw, &parsed)
	res.ID, res.Error = parsed.ID, parsed.Error
	return res, nil
}
