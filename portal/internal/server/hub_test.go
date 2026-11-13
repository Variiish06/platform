package server

import (
	"strings"
	"testing"
)

// EventSource ends a line at "\r\n", "\n" or a lone "\r". A payload with
// bare CRs must not be able to start fields of its own (a forged event).
func TestFormatEventCRInjection(t *testing.T) {
	payload := "hello\r\revent: backup\rdata: x\r\nend\nlast"
	got := formatEvent("notify", payload)

	// Exactly what a browser would parse: split on every kind of line end.
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(got, "\r\n", "\n"), "\r", "\n"), "\n")
	// Expected: one event line, one data line per payload line, then the
	// blank line that ends the event (and the empty remainder after it).
	want := []string{
		"event: notify",
		"data: hello",
		"data: ",
		"data: event: backup",
		"data: data: x",
		"data: end",
		"data: last",
		"",
		"",
	}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q\nwant    %q", lines, want)
	}
	if strings.Contains(got, "\r") {
		t.Fatalf("output still contains a CR: %q", got)
	}
	// Every line but the first and the terminating blank ones is data.
	for _, l := range lines[1 : len(lines)-2] {
		if !strings.HasPrefix(l, "data: ") {
			t.Fatalf("non-data line %q in %q", l, got)
		}
	}
}

func TestFormatEventNameNoLineBreaks(t *testing.T) {
	got := formatEvent("notify\r\nevent: backup\rx", "d")
	if got != "event: notifyevent: backupx\ndata: d\n\n" {
		t.Fatalf("got %q", got)
	}
}
