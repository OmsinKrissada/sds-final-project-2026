//go:build e2e

// End-to-end tests against a running Quizbox stack (local dev or the cluster).
//
//	BASE_URL=http://localhost:8080 go test -tags e2e ./test/e2e/ -v
//	make e2e BASE_URL=http://192.168.0.240
//
// Every HTTP response is checked for X-App-Hops >= 2 (gateway not counted),
// and the WebSocket hello message is checked the same way.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

var base = func() string {
	if v := os.Getenv("BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:8080"
}()

type client struct {
	t *testing.T
	h *http.Client
}

func newClient(t *testing.T) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, h: &http.Client{Jar: jar, Timeout: 10 * time.Second}}
}

// do sends a request, asserts status and hop count, and decodes JSON into out.
func (c *client) do(method, path string, body any, wantStatus int, out any) http.Header {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.h.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		c.t.Fatalf("%s %s: status %d want %d: %s", method, path, resp.StatusCode, wantStatus, data)
	}
	assertHops(c.t, method+" "+path, resp.Header)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("%s %s: decode: %v (%s)", method, path, err, data)
		}
	}
	return resp.Header
}

func assertHops(t *testing.T, what string, h http.Header) {
	t.Helper()
	hops, _ := strconv.Atoi(h.Get("X-App-Hops"))
	trace := h.Get("X-Trace")
	if hops < 2 {
		t.Errorf("%s: X-App-Hops=%d (<2) trace=%q", what, hops, trace)
	} else {
		t.Logf("%-32s hops=%d trace=%s", what, hops, trace)
	}
}

func uniq(prefix string) string { return fmt.Sprintf("%s%d", prefix, rand.Int63()) }

type user struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"displayName"`
}

func register(t *testing.T, c *client, name string) user {
	var out struct{ User user }
	c.do("POST", "/api/auth/register", map[string]string{
		"email": uniq("e2e") + "@test.local", "password": "secret123", "displayName": name,
	}, 201, &out)
	return out.User
}

func TestPagesAndAuth(t *testing.T) {
	c := newClient(t)
	c.do("GET", "/", nil, 200, nil)
	c.do("GET", "/play?pin=000000", nil, 200, nil)
	c.do("GET", "/api/leaderboard?limit=5", nil, 200, nil)

	u := register(t, c, "E2E Host")
	var me struct{ User user }
	c.do("GET", "/api/auth/me", nil, 200, &me)
	if me.User.ID != u.ID {
		t.Fatalf("me=%d want %d", me.User.ID, u.ID)
	}
	c.do("GET", "/api/quizzes", nil, 200, nil)
	c.do("GET", "/api/me/history", nil, 200, nil)
	c.do("POST", "/api/auth/logout", nil, 204, nil)

	// Logged out: protected route is rejected by gateway after authz.
	resp, err := c.h.Get(base + "/api/quizzes")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("after logout status %d want 401", resp.StatusCode)
	}
}

func TestInternalNotExposed(t *testing.T) {
	resp, err := http.Get(base + "/internal/session")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("/internal exposed: status %d", resp.StatusCode)
	}
}

type state struct {
	Type  string `json:"type"`
	V     int    `json:"v"`
	Trace string `json:"trace"`
	Hops  int    `json:"hops"`
	Token string `json:"playerToken"`
	Game  struct {
		Status   string `json:"status"`
		QIndex   int    `json:"qIndex"`
		QCount   int    `json:"qCount"`
		Question *struct {
			Choices []string `json:"choices"`
			Correct *int     `json:"correct"`
		} `json:"question"`
		Reveal *struct {
			Correct int `json:"correct"`
		} `json:"reveal"`
		Scoreboard []struct {
			Nick  string `json:"nick"`
			Score int    `json:"score"`
		} `json:"scoreboard"`
		PlayerCount int `json:"playerCount"`
	} `json:"game"`
}

func wsURL(path string) string {
	return "ws" + strings.TrimPrefix(base, "http") + path
}

func dial(t *testing.T, ctx context.Context, path string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(ctx, wsURL(path), nil)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	conn.SetReadLimit(1 << 20)
	var hello state
	if err := wsjson.Read(ctx, conn, &hello); err != nil || hello.Type != "hello" {
		t.Fatalf("hello: %v %+v", err, hello)
	}
	if hello.Hops < 2 {
		t.Errorf("ws hello hops=%d trace=%q", hello.Hops, hello.Trace)
	}
	return conn
}

// waitFor reads messages until a state satisfies pred.
func waitFor(t *testing.T, ctx context.Context, conn *websocket.Conn, what string, pred func(state) bool) state {
	t.Helper()
	for {
		var s state
		if err := wsjson.Read(ctx, conn, &s); err != nil {
			t.Fatalf("waiting for %s: %v", what, err)
		}
		if s.Type == "state" && pred(s) {
			return s
		}
	}
}

func TestFullGame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	host := newClient(t)
	register(t, host, "E2E Quizmaster")
	var quiz struct{ ID int64 }
	host.do("POST", "/api/quizzes", map[string]any{
		"title": "E2E quiz",
		"questions": []map[string]any{
			{"text": "2+2?", "choices": []string{"3", "4", "5", "22"}, "correct": 1, "timeLimit": 30},
			{"text": "Port for HTTPS?", "choices": []string{"80", "22", "443"}, "correct": 2, "timeLimit": 30},
		},
	}, 201, &quiz)

	var g struct{ Pin, HostToken string }
	host.do("POST", "/api/games", map[string]any{"quizId": quiz.ID}, 201, &g)
	host.do("GET", "/api/games/"+g.Pin, nil, 200, nil)

	hc := dial(t, ctx, "/ws/games/"+g.Pin+"?role=host&token="+g.HostToken)
	defer hc.CloseNow()
	fast := dial(t, ctx, "/ws/games/"+g.Pin+"?role=player&nick=fast")
	defer fast.CloseNow()
	slow := dial(t, ctx, "/ws/games/"+g.Pin+"?role=player&nick=slow")
	defer slow.CloseNow()

	waitFor(t, ctx, hc, "2 players", func(s state) bool { return s.Game.PlayerCount == 2 })
	wsjson.Write(ctx, hc, map[string]string{"type": "start"})

	answers := [][2]int{{1, 0}, {2, 2}} // fast answers correctly both times; slow wrong, then right
	for q, a := range answers {
		st := waitFor(t, ctx, fast, fmt.Sprintf("question %d", q), func(s state) bool { return s.Game.Status == "question" && s.Game.QIndex == q })
		if st.Game.Question != nil && st.Game.Question.Correct != nil {
			t.Fatal("correct answer leaked before reveal")
		}
		wsjson.Write(ctx, fast, map[string]any{"type": "answer", "q": q, "choice": a[0]})
		time.Sleep(300 * time.Millisecond)
		waitFor(t, ctx, slow, fmt.Sprintf("question %d (slow)", q), func(s state) bool { return s.Game.Status == "question" && s.Game.QIndex == q })
		wsjson.Write(ctx, slow, map[string]any{"type": "answer", "q": q, "choice": a[1]})
		wsjson.Write(ctx, hc, map[string]string{"type": "reveal"})
		waitFor(t, ctx, hc, "reveal", func(s state) bool { return s.Game.Status == "reveal" && s.Game.Reveal != nil })
		wsjson.Write(ctx, hc, map[string]string{"type": "next"})
	}
	final := waitFor(t, ctx, hc, "finished", func(s state) bool { return s.Game.Status == "finished" })
	if len(final.Game.Scoreboard) != 2 || final.Game.Scoreboard[0].Nick != "fast" {
		t.Fatalf("unexpected final scoreboard: %+v", final.Game.Scoreboard)
	}
	if final.Game.Scoreboard[0].Score <= final.Game.Scoreboard[1].Score || final.Game.Scoreboard[1].Score == 0 {
		t.Fatalf("scores not ordered as expected: %+v", final.Game.Scoreboard)
	}
	t.Logf("final scoreboard: %+v", final.Game.Scoreboard)

	var hist struct {
		Hosted []struct{ Pin string } `json:"hosted"`
	}
	host.do("GET", "/api/me/history", nil, 200, &hist)
	found := false
	for _, h := range hist.Hosted {
		found = found || h.Pin == g.Pin
	}
	if !found {
		t.Errorf("hosted game %s missing from history", g.Pin)
	}
}
