package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthURL(t *testing.T) {
	cases := map[string]string{
		":8096":          "http://127.0.0.1:8096/healthz",
		"0.0.0.0:8096":   "http://127.0.0.1:8096/healthz",
		"[::]:8096":      "http://127.0.0.1:8096/healthz",
		"10.0.0.5:9000":  "http://10.0.0.5:9000/healthz",
		"localhost:8096": "http://localhost:8096/healthz",
	}
	for listen, want := range cases {
		got, err := healthURL(listen)
		if err != nil || got != want {
			t.Errorf("healthURL(%q) = %q, %v; want %q", listen, got, err, want)
		}
	}
	if _, err := healthURL("8096"); err == nil {
		t.Error("expected error for address without a port separator")
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ok.Close()
	if err := healthcheck(ok.Listener.Addr().String()); err != nil {
		t.Errorf("healthy server: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	if err := healthcheck(bad.Listener.Addr().String()); err == nil {
		t.Error("expected error for 503")
	}

	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // free the port so nothing is listening on it
	if err := healthcheck(addr); err == nil {
		t.Error("expected error when nothing listens")
	}
}

// While the server starts (a schema upgrade can take minutes), clients get
// a 503 saying so and the healthcheck passes; once ready, the router answers.
func TestStartingHandler(t *testing.T) {
	h := &startingHandler{}
	h.starting("blockbustr is upgrading its database")
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil))
		return rec
	}
	rec := get("/Users/AuthenticateByName")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" ||
		rec.Body.String() != "blockbustr is upgrading its database; try again in a minute.\n" {
		t.Errorf("login while upgrading: %d %v %q", rec.Code, rec.Header(), rec.Body)
	}
	if rec := get("/healthz"); rec.Code != http.StatusOK {
		t.Errorf("healthz while upgrading: %d", rec.Code)
	}
	h.ready(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	if rec := get("/Users/AuthenticateByName"); rec.Code != http.StatusTeapot {
		t.Errorf("once ready: %d", rec.Code)
	}
}
