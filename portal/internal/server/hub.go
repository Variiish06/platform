package server

import (
	"strings"
	"sync"
)

// Hub fans events out to every open dashboard (publish/subscribe).
//
// Each browser tab holds one long-lived GET /events response. Background
// loops call Publish; the hub copies the message into every subscriber's
// channel; each subscriber's handler writes it down its own connection.
type Hub struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
	// last remembers the newest message per event name. A new subscriber
	// is sent these at once, so a freshly opened page is current without
	// waiting for the next tick.
	last map[string]string
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{subs: map[chan string]struct{}{}, last: map[string]string{}}
}

// formatEvent encodes one Server-Sent Event. The wire format is plain
// text: an "event:" line, one "data:" line per line of payload, and a
// blank line to finish.
//
// A browser's EventSource ends a line at "\r\n", "\n" OR a lone "\r". The
// payload can carry text from outside (e.g. a notification message), so
// every line ending is normalised to "\n" BEFORE splitting: otherwise a
// "\r" inside the data would start a new line that the browser reads as a
// field of its own ("event: ...", "data: ...", "id: ..."), i.e. a forged
// event. The event name gets no line breaks at all.
func formatEvent(event, data string) string {
	event = strings.NewReplacer("\r", "", "\n", "").Replace(event)
	data = strings.ReplaceAll(data, "\r\n", "\n")
	data = strings.ReplaceAll(data, "\r", "\n")
	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(event)
	b.WriteByte('\n')
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}

// Publish sends an event to all subscribers. If the payload is identical
// to the previous one for this event, nothing is sent: the browser
// already shows it.
func (h *Hub) Publish(event, data string) {
	msg := formatEvent(event, data)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last[event] == msg {
		return
	}
	h.last[event] = msg
	for ch := range h.subs {
		// Never block: one stalled phone must not freeze the loop that
		// feeds everyone else. A full buffer just misses this update;
		// the next one carries the full state again.
		select {
		case ch <- msg:
		default:
		}
	}
}

// Subscribe registers a listener. It returns the channel, the messages
// to send immediately, and a function to unsubscribe.
func (h *Hub) Subscribe() (ch chan string, backlog []string, cancel func()) {
	ch = make(chan string, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	for _, m := range h.last {
		backlog = append(backlog, m)
	}
	h.mu.Unlock()
	return ch, backlog, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// Count is the number of open streams.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// CloseAll ends every stream (used at shutdown).
func (h *Hub) CloseAll() {
	h.mu.Lock()
	for ch := range h.subs {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}
