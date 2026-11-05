package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/quizbox/quizbox/internal/common"
)

const testIndex = `<html><head><!--BOOT--></head><body></body></html>`

type fakes struct {
	account, game *httptest.Server
	gotCookie     string
}

func newFakes(t *testing.T, accountUp, gameUp bool) (*fakes, http.Handler) {
	f := &fakes{}
	f.account = httptest.NewServer(common.TraceMiddleware("account", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotCookie = r.Header.Get("Cookie")
		common.Note(r.Context(), "redis")
		common.WriteJSON(w, 200, map[string]any{"user": map[string]any{"id": 1, "displayName": "</script><b>x"}})
	})))
	f.game = httptest.NewServer(common.TraceMiddleware("game", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		common.Note(r.Context(), "redis")
		switch r.URL.Path {
		case "/api/leaderboard":
			common.WriteJSON(w, 200, map[string]any{"entries": []any{map[string]any{"displayName": "a", "score": 5}}})
		case "/api/games/123456":
			common.WriteJSON(w, 200, map[string]any{"pin": "123456", "status": "lobby"})
		default:
			common.WriteError(w, 404, "no game")
		}
	})))
	t.Cleanup(f.account.Close)
	t.Cleanup(f.game.Close)
	acc, gm := f.account.URL, f.game.URL
	if !accountUp {
		acc = "http://127.0.0.1:1"
	}
	if !gameUp {
		gm = "http://127.0.0.1:1"
	}
	s := &server{account: common.NewClient(acc), game: common.NewClient(gm), index: []byte(testIndex)}
	return f, common.TraceMiddleware("frontend", s.routes())
}

func get(h http.Handler, target string, cookie string) (*http.Response, string) {
	req := httptest.NewRequest("GET", target, nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestHomeBootAndTrace(t *testing.T) {
	f, h := newFakes(t, true, true)
	res, body := get(h, "/", "qb_session=abc")
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if strings.Contains(body, "<!--BOOT-->") || !strings.Contains(body, "<script>window.__BOOT__=") {
		t.Fatalf("boot not injected: %s", body)
	}
	if !strings.Contains(body, `"hallOfFame":{"entries"`) {
		t.Errorf("no hall of fame: %s", body)
	}
	if f.gotCookie != "qb_session=abc" {
		t.Errorf("cookie not forwarded: %q", f.gotCookie)
	}
	// escaping: user-controlled "</script>" must not appear raw
	if strings.Contains(body, "</script><b>") || !strings.Contains(body, `</script`) {
		t.Errorf("not escaped: %s", body)
	}
	tr := res.Header.Get("X-Trace")
	if !strings.Contains(tr, "account@") || !strings.Contains(tr, "game@") || !strings.HasPrefix(tr, "frontend@") {
		t.Errorf("X-Trace = %q", tr)
	}
	if !strings.Contains(body, `"trace":"frontend@`) {
		t.Errorf("trace not in boot: %s", body)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Error("cache-control")
	}
}

func TestPlayGame(t *testing.T) {
	_, h := newFakes(t, true, true)
	_, body := get(h, "/play?pin=123456", "")
	if !strings.Contains(body, `"game":{"pin":"123456"`) {
		t.Errorf("game missing: %s", body)
	}
	_, body = get(h, "/play?pin=999999", "")
	if !strings.Contains(body, `"game":null`) || strings.Contains(body, "degraded\":[\"game") {
		t.Errorf("unknown pin should be null and not degraded: %s", body)
	}
}

func TestGameDown(t *testing.T) {
	_, h := newFakes(t, true, false)
	res, body := get(h, "/", "")
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if !strings.Contains(body, `"hallOfFame":null`) || !strings.Contains(body, `"degraded":["game"]`) {
		t.Errorf("degrade: %s", body)
	}
}

func TestAccountDown(t *testing.T) {
	_, h := newFakes(t, false, true)
	res, body := get(h, "/host", "")
	if res.StatusCode != 503 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if !strings.Contains(body, `"recovering":"account"`) || !strings.Contains(body, "window.__BOOT__") {
		t.Errorf("body: %s", body)
	}
}

func TestExtension404(t *testing.T) {
	_, h := newFakes(t, true, true)
	for _, p := range []string{"/x.js", "/favicon.ico", "/a/b.css"} {
		if res, _ := get(h, p, ""); res.StatusCode != 404 {
			t.Errorf("%s: %d", p, res.StatusCode)
		}
	}
	if res, _ := get(h, "/host/game/123456", ""); res.StatusCode != 200 {
		t.Errorf("spa route: %d", res.StatusCode)
	}
	if res, _ := get(h, "/healthz", ""); res.StatusCode != 200 {
		t.Errorf("healthz: %d", res.StatusCode)
	}
}
