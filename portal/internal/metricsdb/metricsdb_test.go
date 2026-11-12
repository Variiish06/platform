package metricsdb

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakeVM(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL + "/") // trailing slash is trimmed
}

func TestQueryRange(t *testing.T) {
	var got *http.Request
	c := fakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"app":"focal"},"values":[[1700000000,"1"],[1700000060.5,"0.5"],[1700000120,"NaN"]]},
			{"metric":{},"values":[[1700000000,"+Inf"]]}]}}`)
	})
	start := time.Unix(1700000000, 0)
	res, err := c.QueryRange(context.Background(), `avg_over_time(portal_app_up[1d])`, start, start.Add(time.Hour), 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	q := got.URL.Query()
	if got.URL.Path != "/api/v1/query_range" || q.Get("query") != `avg_over_time(portal_app_up[1d])` ||
		q.Get("start") != "1700000000" || q.Get("end") != "1700003600" || q.Get("step") != "90s" || q.Get("nocache") != "1" {
		t.Fatalf("request %s", got.URL)
	}
	if len(res) != 2 || res[0].Labels["app"] != "focal" || len(res[0].Points) != 3 {
		t.Fatalf("result %+v", res)
	}
	if p := res[0].Points[1]; p.V != 0.5 || p.T.UnixMilli() != 1700000060500 {
		t.Fatalf("point %+v", p)
	}
	if !math.IsNaN(res[0].Points[2].V) || !math.IsInf(res[1].Points[0].V, 1) || res[1].Labels == nil {
		t.Fatal("NaN/Inf/empty labels not handled")
	}
}

func TestQueryVector(t *testing.T) {
	c := fakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" || r.URL.Query().Get("time") != "1700000000.25" {
			http.Error(w, "bad", 400)
			return
		}
		io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"name":"a"},"value":[1700000000.25,"42"]}]}}`)
	})
	res, err := c.Query(context.Background(), "x", time.UnixMilli(1700000000250))
	if err != nil || len(res) != 1 || res[0].Points[0].V != 42 {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestErrors(t *testing.T) {
	ctx := context.Background()
	cases := map[string]http.HandlerFunc{
		"api error": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(422)
			io.WriteString(w, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
		},
		"not json": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502); io.WriteString(w, "<html>") },
		"scalar":   func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"status":"success","data":{"resultType":"scalar","result":[]}}`) },
		"bad value": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"abc"]}]}}`)
		},
	}
	for name, h := range cases {
		if _, err := fakeVM(t, h).Query(ctx, "x", time.Now()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}

	// Oversized response: cut off, clear error, not an out-of-memory.
	big := fakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"x":"`)
		chunk := strings.Repeat("a", 64<<10)
		for i := 0; i < (MaxResponseBytes/len(chunk))+2; i++ {
			io.WriteString(w, chunk)
		}
		io.WriteString(w, `"},"value":[1,"1"]}]}}`)
	})
	if _, err := big.Query(ctx, "x", time.Now()); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized: %v", err)
	}

	// Slow server: the caller's deadline wins.
	slow := fakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	tctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := slow.Query(tctx, "x", time.Now()); err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("timeout not honoured: %v after %v", err, time.Since(start))
	}

	// Not configured and unreachable.
	var none *Client = New("")
	if none.Configured() || none.Health(ctx) == nil {
		t.Error("nil client should be unconfigured")
	}
	if _, err := none.Query(ctx, "x", time.Now()); err == nil {
		t.Error("nil client query")
	}
	if _, err := New("http://127.0.0.1:1").QueryRange(ctx, "x", time.Now(), time.Now(), 0); err == nil {
		t.Error("zero step accepted")
	}
}

func TestHealth(t *testing.T) {
	c := fakeVM(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			io.WriteString(w, "OK")
			return
		}
		http.NotFound(w, r)
	})
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}
