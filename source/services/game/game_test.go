package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/coder/websocket"
	_ "github.com/lib/pq"
	"github.com/quizbox/quizbox/internal/common"
	"github.com/redis/go-redis/v9"
)

const (
	testQuizID  = 424242 // owner 42, 3 questions (the only quiz that creates sessions)
	hostUser    = 42
	playerUser  = 900077 // logged-in player linked to the leaderboard
	clockOrigin = int64(1_700_000_000_000)
)

// ---- environment ------------------------------------------------------------------

type env struct {
	t           *testing.T
	mr          *miniredis.Miniredis
	db          *sql.DB
	quizSrv     *httptest.Server
	acctSrv     *httptest.Server
	clock       *atomic.Int64
	accountDown atomic.Bool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, clock: &atomic.Int64{}}
	e.clock.Store(clockOrigin)
	e.mr = miniredis.RunT(t)

	dsn := common.Env("DATABASE_URL", "postgres://quizbox:quizbox@localhost:5432/quizbox?sslmode=disable")
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("postgres not reachable: %v", err)
	}
	e.db = db
	clean := func() {
		_, _ = db.Exec(`DELETE FROM game.sessions WHERE quiz_id=$1`, testQuizID) // results cascade
	}
	clean()
	t.Cleanup(func() { clean(); db.Close() })

	e.quizSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qs := []Question{
			{Text: "Q1", Choices: []string{"a", "b", "c", "d"}, Correct: 1, TimeLimit: 5},
			{Text: "Q2", Choices: []string{"a", "b", "c", "d"}, Correct: 0, TimeLimit: 5},
			{Text: "Q3", Choices: []string{"a", "b", "c"}, Correct: 2, TimeLimit: 5},
		}
		switch r.URL.Path {
		case "/internal/quizzes/424242":
			common.WriteJSON(w, 200, quizDoc{ID: testQuizID, OwnerID: hostUser, Title: "Planets", Questions: qs})
		case "/internal/quizzes/2":
			common.WriteJSON(w, 200, quizDoc{ID: 2, OwnerID: 7, Title: "Other", Questions: qs})
		case "/internal/quizzes/3":
			common.WriteJSON(w, 200, quizDoc{ID: 3, OwnerID: hostUser, Title: "Empty"})
		default:
			common.WriteError(w, 404, "no such quiz")
		}
	}))
	e.acctSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e.accountDown.Load() {
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
		var users []map[string]any
		for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
			if id == fmt.Sprint(playerUser) {
				users = append(users, map[string]any{"id": playerUser, "displayName": "Zed"})
			}
		}
		common.WriteJSON(w, 200, map[string]any{"users": users})
	}))
	t.Cleanup(e.quizSrv.Close)
	t.Cleanup(e.acctSrv.Close)
	return e
}

// newHub starts one replica (own subscriber, own HTTP server) on the shared miniredis.
func (e *env) newHub() (*Hub, *httptest.Server) { return e.newHubWith(e.quizSrv.URL) }

func (e *env) newHubWith(quizURL string) (*Hub, *httptest.Server) {
	rdb := redis.NewClient(&redis.Options{Addr: e.mr.Addr()})
	h := NewHub(rdb, e.db, common.NewClient(quizURL), common.NewClient(e.acctSrv.URL))
	h.now = func() int64 { return e.clock.Load() }
	h.debounce = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	go h.RunSubscriber(ctx)
	srv := httptest.NewServer(common.TraceMiddleware("game", h.Routes()))
	e.t.Cleanup(func() {
		cancel()
		h.CloseAll(4503, "reconnect")
		srv.Close()
		rdb.Close()
	})
	select {
	case <-h.ready:
	case <-time.After(3 * time.Second):
		e.t.Fatal("subscriber never became ready")
	}
	return h, srv
}

// ---- HTTP helpers ---------------------------------------------------------------------

func call(t *testing.T, method, url string, hdr map[string]string, body any) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, url, rd)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func createGame(t *testing.T, srv *httptest.Server) (pin, hostToken string) {
	t.Helper()
	code, out := call(t, "POST", srv.URL+"/api/games", map[string]string{"X-User-Id": fmt.Sprint(hostUser)}, map[string]any{"quizId": testQuizID})
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	return out["pin"].(string), out["hostToken"].(string)
}

// ---- WebSocket helpers --------------------------------------------------------------------

type wsc struct {
	t      *testing.T
	conn   *websocket.Conn
	msgs   chan []byte
	closed chan websocket.CloseError
	leaks  atomic.Int32
	hello  map[string]any
}

func dialWS(t *testing.T, srv *httptest.Server, pin, query string, hdr map[string]string) *wsc {
	t.Helper()
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/games/"+pin+"?"+query, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := &wsc{t: t, conn: conn, msgs: make(chan []byte, 256), closed: make(chan websocket.CloseError, 1)}
	go func() {
		for {
			_, b, err := conn.Read(context.Background())
			if err != nil {
				var ce websocket.CloseError
				if ok := asCloseError(err, &ce); ok {
					c.closed <- ce
				}
				close(c.msgs)
				return
			}
			var m struct {
				Type string `json:"type"`
				Game struct {
					Status string `json:"status"`
				} `json:"game"`
			}
			_ = json.Unmarshal(b, &m)
			// the correct answer must never be on the wire while a question is open
			if m.Type == "state" && m.Game.Status == "question" &&
				(bytes.Contains(b, []byte(`"correct"`)) || bytes.Contains(b, []byte(`"reveal"`)) || bytes.Contains(b, []byte(`lastPoints`))) {
				c.leaks.Add(1)
			}
			c.msgs <- b
		}
	}()
	t.Cleanup(func() { conn.CloseNow() })
	return c
}

func asCloseError(err error, ce *websocket.CloseError) bool {
	for err != nil {
		if e, ok := err.(websocket.CloseError); ok {
			*ce = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// dialOK dials and consumes the hello message.
func dialOK(t *testing.T, srv *httptest.Server, pin, query string, hdr map[string]string) *wsc {
	c := dialWS(t, srv, pin, query, hdr)
	c.hello = c.expect(func(m map[string]any) bool { return m["type"] == "hello" })
	return c
}

func (c *wsc) send(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if err := c.conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// expect returns the first message (dropping non-matching ones) satisfying pred.
func (c *wsc) expect(pred func(map[string]any) bool) map[string]any {
	c.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case b, ok := <-c.msgs:
			if !ok {
				c.t.Fatal("socket closed while waiting")
			}
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			if pred(m) {
				return m
			}
		case <-deadline:
			c.t.Fatal("timed out waiting for message")
		}
	}
}

func game(m map[string]any) map[string]any { return m["game"].(map[string]any) }

func (c *wsc) state(pred func(g map[string]any) bool) map[string]any {
	c.t.Helper()
	m := c.expect(func(m map[string]any) bool { return m["type"] == "state" && pred(game(m)) })
	return game(m)
}

func answered(g map[string]any) bool {
	y, _ := g["you"].(map[string]any)
	return y["answered"] == true
}

func status(s string) func(map[string]any) bool {
	return func(g map[string]any) bool { return g["status"] == s }
}

func (c *wsc) waitClose() websocket.CloseError {
	c.t.Helper()
	select {
	case ce := <-c.closed:
		return ce
	case <-time.After(5 * time.Second):
		c.t.Fatal("expected close")
		return websocket.CloseError{}
	}
}

func intOf(v any) int { f, _ := v.(float64); return int(f) }

// ---- tests ----------------------------------------------------------------------------------

func TestCreateGame(t *testing.T) {
	e := newEnv(t)
	_, srv := e.newHub()
	hdr := map[string]string{"X-User-Id": "42"}

	pin, tok := createGame(t, srv)
	if len(pin) != 6 || tok == "" {
		t.Fatalf("bad pin/token %q %q", pin, tok)
	}
	code, out := call(t, "GET", srv.URL+"/api/games/"+pin, nil, nil)
	if code != 200 || out["status"] != "lobby" || out["quizTitle"] != "Planets" || intOf(out["playerCount"]) != 0 {
		t.Fatalf("get game: %d %v", code, out)
	}
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM game.sessions WHERE pin=$1 AND status='lobby' AND snapshot->'meta'->>'quizTitle'='Planets'`, pin).Scan(&n); err != nil || n != 1 {
		t.Fatalf("session snapshot not stored: %v %d", err, n)
	}
	if code, _ := call(t, "GET", srv.URL+"/api/games/000000", nil, nil); code != 404 {
		t.Fatalf("unknown pin: %d", code)
	}

	for _, tc := range []struct {
		name string
		hdr  map[string]string
		quiz int
		want int
	}{
		{"not owner", hdr, 2, 403},
		{"missing quiz", hdr, 999, 404},
		{"empty quiz", hdr, 3, 400},
		{"no user", nil, testQuizID, 401},
	} {
		if code, out := call(t, "POST", srv.URL+"/api/games", tc.hdr, map[string]any{"quizId": tc.quiz}); code != tc.want {
			t.Errorf("%s: got %d %v want %d", tc.name, code, out, tc.want)
		}
	}

	// quiz service down -> 503 recovering quiz
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	_, srv2 := e.newHubWith(deadURL)
	code, out = call(t, "POST", srv2.URL+"/api/games", hdr, map[string]any{"quizId": testQuizID})
	if code != 503 || out["recovering"] != "quiz" {
		t.Errorf("quiz down: %d %v", code, out)
	}
}

func TestFullGameFlow(t *testing.T) {
	e := newEnv(t)
	_, srv := e.newHub()
	pin, tok := createGame(t, srv)

	host := dialOK(t, srv, pin, "role=host&token="+tok, nil)
	if tr, _ := host.hello["trace"].(string); !strings.Contains(tr, "game@") || !strings.Contains(tr, "redis") || intOf(host.hello["hops"]) < 2 {
		t.Fatalf("bad hello %v", host.hello)
	}
	host.state(status("lobby"))

	ann := dialOK(t, srv, pin, "role=player&nick=ann", nil)
	bob := dialOK(t, srv, pin, "role=player&nick=bob", nil)
	zed := dialOK(t, srv, pin, "role=player&nick=%20ann%20", map[string]string{"X-User-Id": fmt.Sprint(playerUser)})
	if ann.hello["playerId"] == "" || ann.hello["playerToken"] == "" {
		t.Fatalf("player hello %v", ann.hello)
	}
	g := host.state(func(g map[string]any) bool { return intOf(g["playerCount"]) == 3 })
	nicks := map[string]bool{}
	for _, p := range g["players"].([]any) {
		nicks[p.(map[string]any)["nick"].(string)] = true
	}
	if !nicks["ann"] || !nicks["bob"] || !nicks["ann2"] {
		t.Fatalf("nick uniqueness failed: %v", nicks)
	}
	if _, ok := g["you"]; ok {
		t.Fatal("host must not get 'you'")
	}

	// players cannot drive the game
	ann.send(map[string]any{"type": "start"})
	ann.expect(func(m map[string]any) bool { return m["type"] == "error" })
	ann.send(map[string]any{"type": "ping"})
	ann.expect(func(m map[string]any) bool { return m["type"] == "pong" })

	// ---- Q0 (correct = 1, limit 5s) ----
	host.send(map[string]any{"type": "start"})
	host.send(map[string]any{"type": "start"}) // double click: second one is rejected
	host.expect(func(m map[string]any) bool { return m["type"] == "error" })
	q := ann.state(status("question"))
	if intOf(q["qIndex"]) != 0 || intOf(q["qCount"]) != 3 {
		t.Fatalf("q state %v", q)
	}
	qq := q["question"].(map[string]any)
	if intOf(qq["deadline"]) != int(clockOrigin+5000) || intOf(qq["serverNow"]) != int(clockOrigin) {
		t.Fatalf("timing %v", qq)
	}

	e.clock.Store(clockOrigin + 1000)
	ann.send(map[string]any{"type": "answer", "q": 0, "choice": 1}) // 900
	ann.state(func(g map[string]any) bool { return g["you"].(map[string]any)["answered"] == true })
	ann.send(map[string]any{"type": "answer", "q": 0, "choice": 1}) // duplicate
	if m := ann.expect(func(m map[string]any) bool { return m["type"] == "error" }); !strings.Contains(m["error"].(string), "already") {
		t.Fatalf("dup error %v", m)
	}
	ann.send(map[string]any{"type": "answer", "q": 5, "choice": 1}) // wrong q
	ann.expect(func(m map[string]any) bool { return m["type"] == "error" })

	e.clock.Store(clockOrigin + 3000)
	bob.send(map[string]any{"type": "answer", "q": 0, "choice": 1}) // 700
	bob.state(answered)
	zed.send(map[string]any{"type": "answer", "q": 0, "choice": 2}) // wrong
	zed.state(answered)
	host.state(func(g map[string]any) bool { return intOf(g["answered"]) == 3 })
	// while open nobody's score reveals correctness
	g = bob.state(func(g map[string]any) bool { return g["you"].(map[string]any)["answered"] == true })
	if intOf(g["you"].(map[string]any)["score"]) != 0 {
		t.Fatalf("score leaked before reveal: %v", g["you"])
	}

	host.send(map[string]any{"type": "reveal"})
	g = ann.state(status("reveal"))
	rv := g["reveal"].(map[string]any)
	if intOf(rv["correct"]) != 1 || fmt.Sprint(rv["counts"]) != "[0 2 1 0]" {
		t.Fatalf("reveal %v", rv)
	}
	you := g["you"].(map[string]any)
	if intOf(you["score"]) != 900 || intOf(you["lastPoints"]) != 900 || you["lastCorrect"] != true {
		t.Fatalf("ann you %v", you)
	}
	sb := g["scoreboard"].([]any)
	if sb[0].(map[string]any)["nick"] != "ann" || intOf(sb[1].(map[string]any)["score"]) != 700 {
		t.Fatalf("scoreboard %v", sb)
	}

	// ---- Q1 (correct = 0): ann wrong, bob right; then a late answer ----
	host.send(map[string]any{"type": "next"})
	ann.state(func(g map[string]any) bool { return g["status"] == "question" && intOf(g["qIndex"]) == 1 })
	e.clock.Store(clockOrigin + 3000 + 1000)
	bob.send(map[string]any{"type": "answer", "q": 1, "choice": 0}) // 900
	bob.state(answered)
	ann.send(map[string]any{"type": "answer", "q": 1, "choice": 3})
	ann.state(answered)
	e.clock.Store(clockOrigin + 3000 + 5600) // past deadline + 500ms
	zed.send(map[string]any{"type": "answer", "q": 1, "choice": 0})
	if m := zed.expect(func(m map[string]any) bool { return m["type"] == "error" }); !strings.Contains(m["error"].(string), "late") {
		t.Fatalf("late error %v", m)
	}
	host.send(map[string]any{"type": "reveal"})
	host.state(status("reveal"))
	host.send(map[string]any{"type": "reveal"}) // second reveal rejected
	host.expect(func(m map[string]any) bool { return m["type"] == "error" })

	// ---- Q2 (correct = 2) ----
	base := e.clock.Load()
	host.send(map[string]any{"type": "next"})
	ann.state(func(g map[string]any) bool { return g["status"] == "question" && intOf(g["qIndex"]) == 2 })
	e.clock.Store(base + 500) // 950
	zed.send(map[string]any{"type": "answer", "q": 2, "choice": 2})
	zed.state(answered)
	e.clock.Store(base + 2500) // 750
	ann.send(map[string]any{"type": "answer", "q": 2, "choice": 2})
	host.state(func(g map[string]any) bool { return intOf(g["answered"]) == 2 })
	host.send(map[string]any{"type": "reveal"})
	host.state(status("reveal"))
	host.send(map[string]any{"type": "next"})
	g = host.state(status("finished"))
	if g["reveal"] == nil {
		t.Fatal("finished should include reveal")
	}
	// totals: ann 900+0+750=1650, bob 700+900+0=1600, ann2(zed) 0+0+950=950
	sb = g["scoreboard"].([]any)
	var got []string
	for _, s := range sb {
		m := s.(map[string]any)
		got = append(got, fmt.Sprintf("%s:%d", m["nick"], intOf(m["score"])))
	}
	if strings.Join(got, ",") != "ann:1650,bob:1600,ann2:950" {
		t.Fatalf("final scoreboard %v", got)
	}

	for _, c := range []*wsc{host, ann, bob, zed} {
		if n := c.leaks.Load(); n != 0 {
			t.Errorf("%d state messages leaked answer info during a question", n)
		}
	}

	// ---- persistence ----
	rows, err := e.db.Query(`SELECT r.nick, r.score, r.rank, r.player_id FROM game.results r JOIN game.sessions s ON s.id=r.session_id
WHERE s.pin=$1 AND s.status='finished' ORDER BY r.rank`, pin)
	if err != nil {
		t.Fatal(err)
	}
	var res []string
	for rows.Next() {
		var nick string
		var score, rank int
		var pid sql.NullInt64
		rows.Scan(&nick, &score, &rank, &pid)
		res = append(res, fmt.Sprintf("%d:%s:%d:%v", rank, nick, score, pid.Valid))
	}
	rows.Close()
	if strings.Join(res, ",") != "1:ann:1650:false,2:bob:1600:false,3:ann2:950:true" {
		t.Fatalf("results %v", res)
	}
	if s, _ := e.mr.ZScore("lb:alltime", fmt.Sprint(playerUser)); s != 950 {
		t.Fatalf("lb:alltime = %v", s)
	}

	// leaderboard (account up, then down, then rebuilt from PG)
	code, out := call(t, "GET", srv.URL+"/api/leaderboard?limit=5", nil, nil)
	ent := out["entries"].([]any)
	if code != 200 || len(ent) == 0 || ent[0].(map[string]any)["displayName"] != "Zed" || intOf(ent[0].(map[string]any)["score"]) != 950 {
		t.Fatalf("leaderboard %d %v", code, out)
	}
	e.accountDown.Store(true)
	_, out = call(t, "GET", srv.URL+"/api/leaderboard", nil, nil)
	if n := out["entries"].([]any)[0].(map[string]any)["displayName"]; n != fmt.Sprintf("player #%d", playerUser) {
		t.Fatalf("fallback name %v", n)
	}
	e.accountDown.Store(false)
	e.mr.Del("lb:alltime")
	e.mr.Del("lb:alltime:built")
	_, out = call(t, "GET", srv.URL+"/api/leaderboard", nil, nil)
	if ent := out["entries"].([]any); len(ent) == 0 || intOf(ent[0].(map[string]any)["score"]) != 950 || !e.mr.Exists("lb:alltime:built") {
		t.Fatalf("rebuild failed: %v", out)
	}

	// history
	code, out = call(t, "GET", srv.URL+"/api/me/history", map[string]string{"X-User-Id": fmt.Sprint(playerUser)}, nil)
	pl := out["played"].([]any)
	if code != 200 || len(pl) != 1 || pl[0].(map[string]any)["pin"] != pin || intOf(pl[0].(map[string]any)["rank"]) != 3 {
		t.Fatalf("played history %d %v", code, out)
	}
	_, out = call(t, "GET", srv.URL+"/api/me/history", map[string]string{"X-User-Id": "42"}, nil)
	var found bool
	for _, h := range out["hosted"].([]any) {
		if m := h.(map[string]any); m["pin"] == pin && m["status"] == "finished" && m["quizTitle"] == "Planets" {
			found = true
		}
	}
	if !found {
		t.Fatalf("hosted history %v", out)
	}
	if code, _ := call(t, "GET", srv.URL+"/api/me/history", nil, nil); code != 401 {
		t.Fatalf("history without user: %d", code)
	}
}

func TestWSRejections(t *testing.T) {
	e := newEnv(t)
	_, srv := e.newHub()
	pin, _ := createGame(t, srv)
	for _, tc := range []struct {
		pin, q string
		code   int
	}{
		{"123456", "role=host&token=x", 4404},
		{pin, "role=host&token=wrong", 4401},
		{pin, "role=player&resume=nope", 4401},
		{pin, "role=player&nick=", 4400},
		{pin, "role=player&nick=" + strings.Repeat("x", 21), 4400},
		{pin, "role=banana", 4400},
	} {
		c := dialWS(t, srv, tc.pin, tc.q, nil)
		if ce := c.waitClose(); int(ce.Code) != tc.code {
			t.Errorf("%s: close %d want %d", tc.q, ce.Code, tc.code)
		}
	}
}

func TestResume(t *testing.T) {
	e := newEnv(t)
	_, srv := e.newHub()
	pin, tok := createGame(t, srv)
	host := dialOK(t, srv, pin, "role=host&token="+tok, nil)
	ann := dialOK(t, srv, pin, "role=player&nick=ann", nil)
	pid, ptok := ann.hello["playerId"].(string), ann.hello["playerToken"].(string)
	ann.conn.CloseNow()

	host.send(map[string]any{"type": "start"})
	host.state(status("question"))
	ann2 := dialOK(t, srv, pin, "role=player&resume="+ptok, nil)
	if ann2.hello["playerId"] != pid {
		t.Fatalf("resume gave %v want %v", ann2.hello["playerId"], pid)
	}
	g := ann2.state(status("question"))
	if y := g["you"].(map[string]any); y["id"] != pid || y["nick"] != "ann" {
		t.Fatalf("you after resume %v", y)
	}
	if intOf(g["playerCount"]) != 1 {
		t.Fatalf("resume must not add a player: %v", g["playerCount"])
	}
}

func TestMaxPlayers(t *testing.T) {
	e := newEnv(t)
	h, srv := e.newHub()
	pin, _ := createGame(t, srv)
	for i := 0; i < maxPlayers; i++ {
		if _, _, err := h.join(context.Background(), pin, fmt.Sprintf("n%d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	c := dialWS(t, srv, pin, "role=player&nick=late", nil)
	if ce := c.waitClose(); ce.Code != 4400 {
		t.Fatalf("close %v", ce)
	}
}

func TestRedisLossRestore(t *testing.T) {
	e := newEnv(t)
	_, srv := e.newHub()
	pin, tok := createGame(t, srv)
	host := dialOK(t, srv, pin, "role=host&token="+tok, nil)
	ann := dialOK(t, srv, pin, "role=player&nick=ann", nil)
	ptok := ann.hello["playerToken"].(string)
	host.state(func(g map[string]any) bool { return intOf(g["playerCount"]) == 1 })
	host.send(map[string]any{"type": "start"}) // persists snapshot (status question)
	host.state(status("question"))
	time.Sleep(100 * time.Millisecond)

	e.mr.FlushAll() // Redis loses everything
	if e.mr.Exists("game:" + pin + ":meta") {
		t.Fatal("flush failed")
	}

	code, out := call(t, "GET", srv.URL+"/api/games/"+pin, nil, nil)
	if code != 200 || out["status"] != "question" || intOf(out["playerCount"]) != 1 {
		t.Fatalf("restore via GET: %d %v", code, out)
	}
	if !e.mr.Exists("game:" + pin + ":meta") {
		t.Fatal("meta not restored")
	}

	// the game continues: host reveals, player (old token) answers the next question
	host.send(map[string]any{"type": "reveal"})
	host.state(status("reveal"))
	host.send(map[string]any{"type": "next"})
	ann2 := dialOK(t, srv, pin, "role=player&resume="+ptok, nil)
	ann2.state(func(g map[string]any) bool { return g["status"] == "question" && intOf(g["qIndex"]) == 1 })
	ann2.send(map[string]any{"type": "answer", "q": 1, "choice": 0})
	g := ann2.state(func(g map[string]any) bool { return g["you"].(map[string]any)["answered"] == true })
	if intOf(g["answered"]) != 1 {
		t.Fatalf("%v", g)
	}

	// lazy restore on a WS connect too (new host socket after another flush)
	e.mr.FlushAll()
	h2 := dialOK(t, srv, pin, "role=host&token="+tok, nil)
	h2.state(func(g map[string]any) bool { return g["status"] == "question" || g["status"] == "reveal" })

	// a finished game is not restored
	host.send(map[string]any{"type": "end"})
	time.Sleep(200 * time.Millisecond)
	e.mr.FlushAll()
	if code, _ := call(t, "GET", srv.URL+"/api/games/"+pin, nil, nil); code != 404 {
		t.Fatalf("finished+flushed game: %d", code)
	}
}

func TestTwoReplicas(t *testing.T) {
	e := newEnv(t)
	hA, srvA := e.newHub()
	hB, srvB := e.newHub()
	_ = hA
	_ = hB
	pin, tok := createGame(t, srvA)

	host := dialOK(t, srvA, pin, "role=host&token="+tok, nil)
	ann := dialOK(t, srvB, pin, "role=player&nick=ann", nil)
	host.state(func(g map[string]any) bool { return intOf(g["playerCount"]) == 1 }) // join on B reached A's socket

	host.send(map[string]any{"type": "start"})
	ann.state(status("question")) // start on A reached B's socket

	ann.send(map[string]any{"type": "answer", "q": 0, "choice": 1})
	host.state(func(g map[string]any) bool { return intOf(g["answered"]) == 1 }) // answer on B reached A

	// both replicas race the same transition: exactly one wins
	var wins atomic.Int32
	var wg sync.WaitGroup
	for _, h := range []*Hub{hA, hB, hA, hB, hA, hB} {
		wg.Add(1)
		go func(h *Hub) {
			defer wg.Done()
			ctx, _ := common.WithTrace(context.Background(), "t")
			if h.hostAction(ctx, pin, "reveal") == nil {
				wins.Add(1)
			}
		}(h)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("reveal applied %d times", wins.Load())
	}
	v := e.mr.HGet("game:"+pin+":meta", "v")
	host.state(status("reveal"))
	ann.state(status("reveal"))
	if v2 := e.mr.HGet("game:"+pin+":meta", "v"); v2 != v {
		t.Fatalf("v moved %s -> %s", v, v2)
	}
}

func TestRedisRestartResubscribes(t *testing.T) {
	e := newEnv(t)
	_, srvA := e.newHub()
	_, srvB := e.newHub()
	pin, tok := createGame(t, srvA)
	ann := dialOK(t, srvB, pin, "role=player&nick=ann", nil)
	ann.state(status("lobby"))

	e.mr.Close() // drops every connection; Restart keeps the data
	time.Sleep(300 * time.Millisecond)
	if err := e.mr.Restart(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	host := dialOK(t, srvA, pin, "role=host&token="+tok, nil)
	host.send(map[string]any{"type": "start"})
	// replica B reconnected its subscriber (or refreshed after reconnect) and pushes the new state
	ann.state(status("question"))
}

func TestShutdownClosesSockets(t *testing.T) {
	e := newEnv(t)
	h, srv := e.newHub()
	pin, _ := createGame(t, srv)
	ann := dialOK(t, srv, pin, "role=player&nick=ann", nil)
	h.CloseAll(4503, "reconnect")
	if ce := ann.waitClose(); ce.Code != 4503 || ce.Reason != "reconnect" {
		t.Fatalf("close %v", ce)
	}
}

func TestMain(m *testing.M) { os.Exit(m.Run()) }
