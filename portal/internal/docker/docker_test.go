package docker

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
)

// fakeEngine serves HTTP on a unix socket, like the socket proxy.
func fakeEngine(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return New(sock)
}

func TestStopRequest(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var got []string
	status := http.StatusNoContent
	c := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.RequestURI())
		w.WriteHeader(status)
	})
	if err := c.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if want := "POST /" + apiVersion + "/containers/" + id + "/stop?t=10"; len(got) != 1 || got[0] != want {
		t.Fatalf("got %v, want %s", got, want)
	}
	// 304: already stopped, which is what we wanted.
	status = http.StatusNotModified
	if err := c.Stop(context.Background(), id); err != nil {
		t.Errorf("304: %v", err)
	}
	// The proxy's refusal is an error.
	status = http.StatusForbidden
	if err := c.Stop(context.Background(), id); err == nil {
		t.Error("403 not reported")
	}
	// Nothing that is not a container id ever reaches the socket.
	n := len(got)
	for _, bad := range []string{"", "focal", "../../images", id + "/../x"} {
		if err := c.Stop(context.Background(), bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if len(got) != n {
		t.Error("invalid id was sent")
	}
}
