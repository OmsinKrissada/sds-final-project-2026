package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/quizbox/quizbox/internal/common"
)

// fake upstream: replies with JSON describing what it saw and a fixed X-Trace.
func fake(name, trace string, hits *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.Header().Set("X-Trace", trace)
		common.WriteJSON(w, 200, map[string]string{
			"svc": name, "path": r.URL.Path, "uid": r.Header.Get("X-User-Id"),
			"uname": r.Header.Get("X-User-Name"), "host": r.Host, "xff": r.Header.Get("X-Forwarded-For"),
		})
	}))
}

func fakeAccount(hits *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Trace", "account@n2,redis")
		if r.URL.Path != "/internal/authz" {
			common.WriteJSON(w, 200, map[string]string{"svc": "account", "path": r.URL.Path, "uid": r.Header.Get("X-User-Id")})
			return
		}
		hits.Add(1)
		if c, err := r.Cookie("qb_session"); err != nil || c.Value != "good" {
			common.WriteError(w, 401, "no")
			return
		}
		w.Header().Set("X-User-Id", "7")
		w.Header().Set("X-User-Name", "Ann")
		common.WriteJSON(w, 200, map[string]any{"user": 7})
	}))
}

func closedURL(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "http://" + l.Addr().String()
	l.Close()
	return u
}

type env struct {
	gw                         *httptest.Server
	authzHits, quizHits        atomic.Int32
	gameHits, frontHits        atomic.Int32
	account, quiz, game, front *httptest.Server
}

func setup(t *testing.T, accountURL, quizURL string) *env {
	t.Helper()
	t.Setenv("NODE_NAME", "n0")
	e := &env{}
	e.account = fakeAccount(&e.authzHits)
	e.quiz = fake("quiz", "quiz@n1,postgres", &e.quizHits)
	e.game = fake("game", "game@n3,redis", &e.gameHits)
	e.front = fake("frontend", "frontend@n1", &e.frontHits)
	t.Cleanup(func() { e.account.Close(); e.quiz.Close(); e.game.Close(); e.front.Close() })
	if accountURL == "" {
		accountURL = e.account.URL
	}
	if quizURL == "" {
		quizURL = e.quiz.URL
	}
	h, err := newGateway(accountURL, quizURL, e.game.URL, e.front.URL)
	if err != nil {
		t.Fatal(err)
	}
	e.gw = httptest.NewServer(common.TraceMiddleware("gateway", h))
	t.Cleanup(e.gw.Close)
	return e
}

type result struct {
	code int
	hdr  http.Header
	body map[string]string
}

func (e *env) do(t *testing.T, method, path string, hdr map[string]string) result {
	t.Helper()
	req, _ := http.NewRequest(method, e.gw.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	r := result{code: resp.StatusCode, hdr: resp.Header, body: map[string]string{}}
	_ = json.Unmarshal(b, &r.body)
	return r
}

var good = map[string]string{"Cookie": "qb_session=good"}

func TestRouting(t *testing.T) {
	e := setup(t, "", "")
	cases := []struct {
		method, path string
		hdr          map[string]string
		code         int
		svc          string
	}{
		{"POST", "/api/auth/login", nil, 200, "account"},
		{"GET", "/api/quizzes", good, 200, "quiz"},
		{"GET", "/api/quizzes/5", good, 200, "quiz"},
		{"GET", "/api/quizzes", nil, 401, ""},
		{"GET", "/api/quizzes", map[string]string{"Cookie": "qb_session=bad"}, 401, ""},
		{"POST", "/api/games", good, 200, "game"},
		{"POST", "/api/games", nil, 401, ""},
		{"GET", "/api/games/123456", nil, 200, "game"},
		{"GET", "/api/leaderboard", nil, 200, "game"},
		{"GET", "/api/me/history", good, 200, "game"},
		{"GET", "/api/me/history", nil, 401, ""},
		{"GET", "/ws/games/1", nil, 200, "game"},
		{"GET", "/", nil, 200, "frontend"},
		{"GET", "/play", nil, 200, "frontend"},
		{"GET", "/internal/authz", nil, 404, ""},
		{"GET", "/internal/users", nil, 404, ""},
	}
	for _, c := range cases {
		r := e.do(t, c.method, c.path, c.hdr)
		if r.code != c.code || (c.svc != "" && r.body["svc"] != c.svc) {
			t.Errorf("%s %s: got %d %v, want %d %s", c.method, c.path, r.code, r.body, c.code, c.svc)
		}
		if c.code == 401 && r.body["error"] != "login required" {
			t.Errorf("%s %s: body %v", c.method, c.path, r.body)
		}
	}
	if r := e.do(t, "GET", "/healthz", nil); r.code != 200 || r.body["node"] != "n0" {
		t.Errorf("healthz: %d %v", r.code, r.body)
	}
	// authz must not be consulted for public routes.
	before := e.authzHits.Load()
	e.do(t, "GET", "/api/games/1", nil)
	e.do(t, "GET", "/", nil)
	if e.authzHits.Load() != before {
		t.Error("authz called for public route")
	}
}

func TestHeaderStripAndInject(t *testing.T) {
	e := setup(t, "", "")
	spoof := map[string]string{"X-User-Id": "999", "X-User-Name": "evil"}
	if r := e.do(t, "GET", "/api/games/1", spoof); r.body["uid"] != "" || r.body["uname"] != "" {
		t.Errorf("public route leaked spoofed identity: %v", r.body)
	}
	spoof["Cookie"] = "qb_session=good"
	if r := e.do(t, "GET", "/api/quizzes", spoof); r.body["uid"] != "7" || r.body["uname"] != "Ann" {
		t.Errorf("want injected 7/Ann, got %v", r.body)
	}
	// optional: valid session injected, invalid session anonymous (spoof removed).
	if r := e.do(t, "GET", "/ws/x", spoof); r.body["uid"] != "7" {
		t.Errorf("ws optional inject: %v", r.body)
	}
	if r := e.do(t, "GET", "/ws/x", map[string]string{"X-User-Id": "999"}); r.code != 200 || r.body["uid"] != "" {
		t.Errorf("ws anonymous: %d %v", r.code, r.body)
	}
	// forwarded headers set; Host preserved.
	r := e.do(t, "GET", "/", nil)
	if r.body["xff"] == "" || !strings.HasPrefix(r.body["host"], "127.0.0.1:") || r.body["host"] != strings.TrimPrefix(e.gw.URL, "http://") {
		t.Errorf("forwarding headers: %v", r.body)
	}
}

func TestTraceMerge(t *testing.T) {
	e := setup(t, "", "")
	r := e.do(t, "GET", "/api/quizzes", good)
	if got, want := r.hdr.Values("X-Trace"), []string{"gateway@n0,account@n2,redis,quiz@n1,postgres"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("X-Trace = %v, want %v", got, want)
	}
	if h := r.hdr.Get("X-App-Hops"); h != "3" {
		t.Errorf("X-App-Hops = %q, want 3", h)
	}
	// public route: no authz hops.
	r = e.do(t, "GET", "/api/games/1", nil)
	if got := r.hdr.Get("X-Trace"); got != "gateway@n0,game@n3,redis" || r.hdr.Get("X-App-Hops") != "2" {
		t.Errorf("public trace %q hops %q", got, r.hdr.Get("X-App-Hops"))
	}
	// 401 still shows account hop.
	r = e.do(t, "GET", "/api/quizzes", nil)
	if got := r.hdr.Get("X-Trace"); got != "gateway@n0,account@n2,redis" || r.hdr.Get("X-App-Hops") != "2" {
		t.Errorf("401 trace %q hops %q", got, r.hdr.Get("X-App-Hops"))
	}
}

func TestUnavailable(t *testing.T) {
	// quiz down
	e := setup(t, "", closedURL(t))
	start := time.Now()
	r := e.do(t, "GET", "/api/quizzes", good)
	if r.code != 503 || r.body["recovering"] != "quiz" || r.body["error"] != "service unavailable" {
		t.Errorf("quiz down: %d %v", r.code, r.body)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("expected one 150ms retry for GET, took %v", d)
	}
	if r.hdr.Get("X-Trace") != "gateway@n0,account@n2,redis" {
		t.Errorf("503 trace %q", r.hdr.Get("X-Trace"))
	}
	// non-idempotent: no retry (fast), still 503.
	start = time.Now()
	if r := e.do(t, "POST", "/api/quizzes", good); r.code != 503 || r.body["recovering"] != "quiz" {
		t.Errorf("POST quiz down: %d %v", r.code, r.body)
	} else if time.Since(start) >= 150*time.Millisecond {
		t.Errorf("POST should not retry")
	}

	// account down
	e = setup(t, closedURL(t), "")
	r = e.do(t, "GET", "/api/quizzes", good)
	if r.code != 503 || r.body["recovering"] != "account" {
		t.Errorf("account down required: %d %v", r.code, r.body)
	}
	// optional route continues anonymously; public routes unaffected.
	if r := e.do(t, "GET", "/ws/x", good); r.code != 200 || r.body["uid"] != "" {
		t.Errorf("account down optional: %d %v", r.code, r.body)
	}
	if r := e.do(t, "GET", "/api/leaderboard", nil); r.code != 200 {
		t.Errorf("public with account down: %d", r.code)
	}
}

func TestWebSocketPassthrough(t *testing.T) {
	t.Setenv("NODE_NAME", "n0")
	account := fakeAccount(new(atomic.Int32))
	defer account.Close()
	game := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Trace", "game@n3,redis")
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		for {
			typ, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			_ = c.Write(ctx, typ, []byte(r.Header.Get("X-User-Id")+":"+string(b)))
		}
	}))
	defer game.Close()
	h, err := newGateway(account.URL, closedURL(t), game.URL, closedURL(t))
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(common.TraceMiddleware("gateway", h))
	defer gw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(gw.URL, "http")+"/ws/games/123456?role=player&nick=ann",
		&websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {"qb_session=good"}, "X-User-Id": {"999"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if resp.StatusCode != 101 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := resp.Header.Values("X-Trace"); len(got) != 1 || got[0] != "gateway@n0,account@n2,redis,game@n3" {
		t.Errorf("101 X-Trace = %v", got)
	}
	if got := resp.Header.Get("X-App-Hops"); got != "3" {
		t.Errorf("101 X-App-Hops = %q, want 3", got)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	_, b, err := c.Read(ctx)
	if err != nil || string(b) != "7:hi" {
		t.Fatalf("echo = %q, %v", b, err)
	}
	c.Close(websocket.StatusNormalClosure, "")
}
