package common

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAppHops(t *testing.T) {
	cases := map[string]int{
		"gateway@pi-2,account@pi-3,redis,quiz@pi-1,postgres": 3,
		"gateway@pi-2,frontend@pi-1":                         1,
		"account@a,account@b,redis":                          2,
		"":                                                   0,
	}
	for in, want := range cases {
		if got := AppHops(in); got != want {
			t.Errorf("AppHops(%q)=%d want %d", in, got, want)
		}
	}
}

func TestTraceMergesDownstream(t *testing.T) {
	t.Setenv("NODE_NAME", "n1")
	down := httptest.NewServer(TraceMiddleware("quiz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Note(r.Context(), "postgres")
		WriteJSON(w, 200, map[string]int{"ok": 1})
	})))
	defer down.Close()
	c := NewClient(down.URL)
	up := httptest.NewServer(TraceMiddleware("game", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Note(r.Context(), "redis")
		var out map[string]int
		if err := c.GetJSON(r.Context(), "/", nil, &out); err != nil {
			t.Error(err)
		}
		w.WriteHeader(204)
	})))
	defer up.Close()
	resp, err := http.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resp.Header.Get(HeaderTrace), "game@n1,redis,quiz@n1,postgres"; got != want {
		t.Fatalf("trace %q want %q", got, want)
	}
}

func TestClientUnavailable(t *testing.T) {
	c := NewClient("http://127.0.0.1:1")
	err := c.GetJSON(context.Background(), "/", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}
