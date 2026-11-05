package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/quizbox/quizbox/internal/common"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := hashPassword("hunter22")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$60000$") {
		t.Fatalf("unexpected format: %s", h)
	}
	if !verifyPassword("hunter22", h) {
		t.Fatal("correct password rejected")
	}
	if verifyPassword("hunter23", h) {
		t.Fatal("wrong password accepted")
	}
	h2, _ := hashPassword("hunter22")
	if h == h2 {
		t.Fatal("salt is not random")
	}
	for _, bad := range []string{"", "pbkdf2-sha256$x$a$b", "md5$1$a$b", "pbkdf2-sha256$1$!!$!!", "a$b"} {
		if verifyPassword("hunter22", bad) {
			t.Fatalf("malformed hash %q verified", bad)
		}
	}
}

func TestValidateRegister(t *testing.T) {
	cases := []struct {
		email, pw, name string
		ok              bool
	}{
		{"a@b.c", "123456", "Ann", true},
		{"nobody", "123456", "Ann", false},
		{"@b.c", "123456", "Ann", false},
		{"a@", "123456", "Ann", false},
		{"a@b.c", "12345", "Ann", false},
		{"a@b.c", "123456", "", false},
		{"a@b.c", "123456", strings.Repeat("x", 41), false},
		{"a@b.c", "123456", "A\nB", false},
	}
	for _, c := range cases {
		if msg := validateRegister(c.email, c.pw, c.name); (msg == "") != c.ok {
			t.Errorf("validateRegister(%q,%q,%q) = %q, want ok=%v", c.email, c.pw, c.name, msg, c.ok)
		}
	}
}

// harness runs the account handlers on an httptest server against the real
// local Postgres and a miniredis instance. Skips when Postgres is unreachable.
type harness struct {
	db  *sql.DB
	mr  *miniredis.Miniredis
	st  *store
	srv *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := sql.Open("postgres", common.Env("DATABASE_URL",
		"postgres://quizbox:quizbox@localhost:5432/quizbox?sslmode=disable"))
	if err != nil {
		t.Skipf("postgres: %v", err)
	}
	db.SetMaxOpenConns(5)
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{
		Addr:         mr.Addr(),
		DialTimeout:  200 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { rdb.Close() })

	st := &store{db: db, rdb: rdb}
	srv := &server{st: st}
	mux := http.NewServeMux()
	srv.routes(mux)
	ts := httptest.NewServer(common.TraceMiddleware("account", mux))
	t.Cleanup(ts.Close)
	return &harness{db: db, mr: mr, st: st, srv: ts}
}

// uniqueEmail returns a random address and deletes its user (and sessions)
// when the test ends.
func (h *harness) uniqueEmail(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	email := "test-" + hex.EncodeToString(b) + "@test.quizbox.local"
	t.Cleanup(func() {
		if _, err := h.db.Exec(`DELETE FROM account.users WHERE email = $1`, email); err != nil {
			t.Logf("cleanup %s: %v", email, err)
		}
	})
	return email
}

func (h *harness) call(t *testing.T, method, path, cookie string, body any) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// sessionCookie returns "qb_session=<token>" from a response, or "".
func sessionCookie(resp *http.Response) string {
	for _, c := range resp.Cookies() {
		if c.Name == cookieName && c.Value != "" {
			return cookieName + "=" + c.Value
		}
	}
	return ""
}

func (h *harness) register(t *testing.T, email, name string) (User, string) {
	t.Helper()
	resp, body := h.call(t, "POST", "/api/auth/register", "", map[string]string{
		"email": email, "password": "secret1", "displayName": name,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %s", resp.StatusCode, body)
	}
	var out struct {
		User User `json:"user"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out.User, sessionCookie(resp)
}

func TestAuthFlow(t *testing.T) {
	h := newHarness(t)
	email := h.uniqueEmail(t)

	u, cookie := h.register(t, email, "Tester")
	if cookie == "" {
		t.Fatal("register set no session cookie")
	}
	if u.ID == 0 || u.Email != email || u.DisplayName != "Tester" {
		t.Fatalf("unexpected user: %+v", u)
	}

	if resp, _ := h.call(t, "POST", "/api/auth/register", "", map[string]string{
		"email": email, "password": "secret1", "displayName": "Dup",
	}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate register: want 409, got %d", resp.StatusCode)
	}

	resp, _ := h.call(t, "GET", "/internal/authz", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authz: %d", resp.StatusCode)
	}
	if got := resp.Header.Get(common.HeaderUserID); got != strconv.FormatInt(u.ID, 10) {
		t.Fatalf("X-User-Id = %q", got)
	}
	if got := resp.Header.Get(common.HeaderUser); got != "Tester" {
		t.Fatalf("X-User-Name = %q", got)
	}
	trace := resp.Header.Get(common.HeaderTrace)
	if !strings.Contains(trace, "redis") || strings.Contains(trace, "postgres") {
		t.Fatalf("authz on a warm session should touch redis only, trace=%q", trace)
	}

	if resp, body := h.call(t, "GET", "/api/auth/me", cookie, nil); resp.StatusCode != http.StatusOK ||
		!strings.Contains(string(body), email) {
		t.Fatalf("me: %d %s", resp.StatusCode, body)
	}

	if resp, _ := h.call(t, "POST", "/api/auth/login", "", map[string]string{
		"email": email, "password": "wrong-pass",
	}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad password: want 401, got %d", resp.StatusCode)
	}
	resp, body := h.call(t, "POST", "/api/auth/login", "", map[string]string{
		"email": email, "password": "secret1",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %s", resp.StatusCode, body)
	}
	loginCookie := sessionCookie(resp)
	if loginCookie == "" {
		t.Fatal("login set no cookie")
	}

	resp, _ = h.call(t, "POST", "/api/auth/logout", loginCookie, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp, _ := h.call(t, "GET", "/internal/authz", loginCookie, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("authz after logout: want 401, got %d", resp.StatusCode)
	}
	tok := strings.TrimPrefix(loginCookie, cookieName+"=")
	var n int
	if err := h.db.QueryRow(`SELECT count(*) FROM account.sessions WHERE token_hash = $1`, tokenHash(tok)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("session row after logout: n=%d err=%v", n, err)
	}
	if h.mr.Exists(sessionKey(tokenHash(tok))) {
		t.Fatal("redis session survived logout")
	}

	if resp, body := h.call(t, "GET", "/internal/session", "", nil); resp.StatusCode != http.StatusOK ||
		string(bytes.TrimSpace(body)) != `{"user":null}` {
		t.Fatalf("session without cookie: %d %s", resp.StatusCode, body)
	}
	if _, body := h.call(t, "GET", "/internal/session", cookie, nil); !strings.Contains(string(body), email) {
		t.Fatalf("session with cookie: %s", body)
	}

	_, body = h.call(t, "GET", "/internal/users?ids="+strconv.FormatInt(u.ID, 10), "", nil)
	var users struct {
		Users []map[string]any `json:"users"`
	}
	if err := json.Unmarshal(body, &users); err != nil || len(users.Users) != 1 ||
		users.Users[0]["displayName"] != "Tester" || users.Users[0]["email"] != nil {
		t.Fatalf("users: %s (%v)", body, err)
	}
}

func TestRedisDownFallsBackToPostgres(t *testing.T) {
	h := newHarness(t)
	email := h.uniqueEmail(t)
	u, cookie := h.register(t, email, "Fallback")
	key := sessionKey(tokenHash(strings.TrimPrefix(cookie, cookieName+"=")))
	if !h.mr.Exists(key) {
		t.Fatal("register did not cache the session in redis")
	}

	// Redis cache lost: PG answers and repopulates the cache.
	h.mr.FlushAll()
	if resp, _ := h.call(t, "GET", "/internal/authz", cookie, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("authz after cache flush: %d", resp.StatusCode)
	}
	if !h.mr.Exists(key) {
		t.Fatal("cache not repopulated from postgres")
	}

	// Redis gone entirely: every path still works through Postgres.
	h.mr.Close()
	resp, _ := h.call(t, "GET", "/internal/authz", cookie, nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get(common.HeaderUserID) != strconv.FormatInt(u.ID, 10) {
		t.Fatalf("authz with redis down: %d", resp.StatusCode)
	}
	if trace := resp.Header.Get(common.HeaderTrace); !strings.Contains(trace, "postgres") || strings.Contains(trace, "redis") {
		t.Fatalf("trace with redis down: %q", trace)
	}
	if resp, _ := h.call(t, "GET", "/api/auth/me", cookie, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("me with redis down: %d", resp.StatusCode)
	}
	if resp, _ := h.call(t, "POST", "/api/auth/logout", cookie, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout with redis down: %d", resp.StatusCode)
	}
	if resp, _ := h.call(t, "GET", "/internal/authz", cookie, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("authz after logout with redis down: want 401, got %d", resp.StatusCode)
	}
}
