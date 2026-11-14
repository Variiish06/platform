package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// silencer keeps Alertmanager quiet about things that were stopped on
// purpose from the dashboard.
//
// Stopping a container from Portal creates a silence for its alerts
// (matcher name="<container>", and app="<id>" when it belongs to an app
// in apps.yaml), createdBy "portal", for 30 days. Starting it again from
// Portal expires those silences. Alertmanager being down never blocks a
// stop or a start: the caller logs the error and shows a warning.
//
// Silence IDs are kept in memory, and on expiry Portal also asks
// Alertmanager for active silences created by "portal" with exactly that
// one matcher, so a Portal restart in between does not leave a silence
// behind.
type silencer struct {
	base   string // e.g. http://127.0.0.1:9093; "" disables
	client *http.Client

	mu  sync.Mutex
	ids map[string]string // "name=focal" -> silence id
}

const (
	silenceDuration = 30 * 24 * time.Hour
	silenceCreator  = "portal"
	silenceTimeout  = 5 * time.Second
	// silenceGrace: how long a silence lives on after a start from Portal
	// (see Unsilence).
	silenceGrace = 3 * time.Minute
)

// Alertmanager silence IDs are UUIDs. Checked before one goes into a URL.
var silenceIDRe = regexp.MustCompile(`^[a-f0-9-]{8,64}$`)

func newSilencer(base string) *silencer {
	return &silencer{
		base:   strings.TrimRight(base, "/"),
		client: &http.Client{Timeout: silenceTimeout},
		ids:    map[string]string{},
	}
}

// enabled reports whether an Alertmanager URL is configured.
func (s *silencer) enabled() bool { return s != nil && s.base != "" }

type amMatcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

// silenceKey is the matcher a silence is about, e.g. name=focal.
type silenceKey struct{ Label, Value string }

func (k silenceKey) String() string { return k.Label + "=" + k.Value }

// Silence creates a 30-day silence for one exact matcher and returns its id.
func (s *silencer) Silence(ctx context.Context, k silenceKey, comment string) (string, error) {
	if !s.enabled() {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*silenceTimeout)
	defer cancel()
	// Stopping something already stopped (and silenced) must not pile up
	// duplicate silences: reuse a live one Portal made earlier.
	if found, err := s.lookup(ctx, k); err == nil && len(found) > 0 {
		s.mu.Lock()
		s.ids[k.String()] = found[0].ID
		s.mu.Unlock()
		return found[0].ID, nil
	}
	now := time.Now().UTC()
	body, _ := json.Marshal(map[string]any{
		"matchers":  []amMatcher{{Name: k.Label, Value: k.Value, IsEqual: true}},
		"startsAt":  now.Format(time.RFC3339),
		"endsAt":    now.Add(silenceDuration).Format(time.RFC3339),
		"createdBy": silenceCreator,
		"comment":   comment,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/api/v2/silences", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("alertmanager: create silence: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		SilenceID string `json:"silenceID"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", fmt.Errorf("alertmanager: create silence: %w", err)
	}
	s.mu.Lock()
	s.ids[k.String()] = out.SilenceID
	s.mu.Unlock()
	return out.SilenceID, nil
}

// Unsilence ends every active silence Portal created for exactly this
// matcher, including ones from before a Portal restart. Returns how many.
//
// It does not delete them outright: each silence's end is moved to
// silenceGrace from now. Right after a start, vmalert still reports the
// container as down for up to a minute (scrape + evaluation interval); a
// silence deleted at once would let Alertmanager send that stale
// "ContainerDown" to the phone at its next group flush. Moving endsAt is
// stored in Alertmanager, so it survives a Portal restart. If Alertmanager
// cannot be listed or refuses the update, the silence is deleted instead.
func (s *silencer) Unsilence(ctx context.Context, k silenceKey) (int, error) {
	if !s.enabled() {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*silenceTimeout)
	defer cancel()

	// Alertmanager's own list is authoritative (it skips silences someone
	// already expired by hand). Only if it cannot be listed do we fall
	// back to the id we remember.
	found, lookupErr := s.lookup(ctx, k)
	if lookupErr != nil {
		s.mu.Lock()
		if id, ok := s.ids[k.String()]; ok {
			found = append(found, amSilence{ID: id})
		}
		s.mu.Unlock()
	}
	n := 0
	var firstErr error
	for _, sl := range found {
		err := s.windDown(ctx, sl)
		if err != nil {
			err = s.expire(ctx, sl.ID)
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n++
	}
	if firstErr == nil && lookupErr == nil {
		s.mu.Lock()
		delete(s.ids, k.String())
		s.mu.Unlock()
	}
	if firstErr == nil {
		firstErr = lookupErr
	}
	return n, firstErr
}

// windDown moves a silence's end to silenceGrace from now (an update:
// POST with the silence's id).
func (s *silencer) windDown(ctx context.Context, sl amSilence) error {
	if sl.StartsAt.IsZero() || len(sl.Matchers) == 0 {
		return fmt.Errorf("alertmanager: silence %s not listed", sl.ID)
	}
	body, _ := json.Marshal(map[string]any{
		"id":        sl.ID,
		"matchers":  sl.Matchers,
		"startsAt":  sl.StartsAt.UTC().Format(time.RFC3339Nano),
		"endsAt":    time.Now().UTC().Add(silenceGrace).Format(time.RFC3339),
		"createdBy": silenceCreator,
		"comment":   sl.Comment + "; started again from Portal",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/api/v2/silences", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("alertmanager: update silence %s: %s", sl.ID, resp.Status)
	}
	return nil
}

// lookup lists active silences created by Portal whose only matcher is k.
func (s *silencer) lookup(ctx context.Context, k silenceKey) ([]amSilence, error) {
	q := url.Values{"filter": {fmt.Sprintf("%s=%q", k.Label, k.Value)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/api/v2/silences?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("alertmanager: list silences: %s", resp.Status)
	}
	var list []amSilence
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list); err != nil {
		return nil, fmt.Errorf("alertmanager: list silences: %w", err)
	}
	var out []amSilence
	for _, sl := range list {
		if got, ok := sl.portalKey(); ok && got == k {
			out = append(out, sl)
		}
	}
	return out, nil
}

func (s *silencer) expire(ctx context.Context, id string) error {
	if !silenceIDRe.MatchString(id) {
		return fmt.Errorf("alertmanager: odd silence id %q", id)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.base+"/api/v2/silence/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("alertmanager: expire silence %s: %s", id, resp.Status)
	}
	return nil
}

// Load replaces the remembered silences with the active ones Portal
// created, as Alertmanager lists them. Run at start-up (memory is empty
// after a Portal restart) and every few minutes (a silence expired by
// hand in Alertmanager's UI is then forgotten here too).
func (s *silencer) Load(ctx context.Context) error {
	if !s.enabled() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, silenceTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/api/v2/silences", nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("alertmanager: list silences: %s", resp.Status)
	}
	var list []amSilence
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list); err != nil {
		return fmt.Errorf("alertmanager: list silences: %w", err)
	}
	ids := map[string]string{}
	for _, sl := range list {
		if k, ok := sl.portalKey(); ok {
			ids[k.String()] = sl.ID
		}
	}
	s.mu.Lock()
	s.ids = ids
	s.mu.Unlock()
	return nil
}

// amSilence is one entry of GET /api/v2/silences.
type amSilence struct {
	ID        string      `json:"id"`
	CreatedBy string      `json:"createdBy"`
	Comment   string      `json:"comment"`
	Matchers  []amMatcher `json:"matchers"`
	StartsAt  time.Time   `json:"startsAt"`
	EndsAt    time.Time   `json:"endsAt"`
	Status    struct {
		State string `json:"state"`
	} `json:"status"`
}

// portalKey returns the matcher of a live silence Portal made (one exact
// matcher, createdBy "portal", not expired and not already winding down
// after a start).
func (sl amSilence) portalKey() (silenceKey, bool) {
	if sl.CreatedBy != silenceCreator || sl.Status.State == "expired" || len(sl.Matchers) != 1 {
		return silenceKey{}, false
	}
	if !sl.EndsAt.IsZero() && sl.EndsAt.Before(time.Now().Add(silenceGrace+time.Minute)) {
		return silenceKey{}, false
	}
	m := sl.Matchers[0]
	if m.IsRegex || !m.IsEqual {
		return silenceKey{}, false
	}
	return silenceKey{m.Name, m.Value}, true
}
