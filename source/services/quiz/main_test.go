package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/quizbox/quizbox/internal/common"
)

// Tests run against the real local Postgres. Owners are random large ids so
// they never collide with real data, and rows are deleted on cleanup.

func testHandler(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()
	db, err := openDB(common.Env("DATABASE_URL", defaultDSN))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("postgres: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return common.TraceMiddleware("quiz", newMux(db)), db
}

func randOwner() int64 { return 900000000 + rand.Int64N(100000000) }

// call sends a request; uid "" omits X-User-Id; body "" sends no body.
func call(t *testing.T, h http.Handler, method, path, uid, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if uid != "" {
		req.Header.Set(common.HeaderUserID, uid)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

func expectCode(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status %d, want %d; body %s", rec.Code, want, rec.Body.String())
	}
}

const goodQ = `{"text":"Largest planet?","choices":["Mars","Jupiter","Venus","Earth"],"correct":1}`

func TestQuizCRUD(t *testing.T) {
	h, db := testHandler(t)
	owner := randOwner()
	other := owner + 1
	t.Cleanup(func() { db.Exec(`DELETE FROM quiz.quizzes WHERE owner_id IN ($1, $2)`, owner, other) })
	me, them := fmt.Sprint(owner), fmt.Sprint(other)

	// Auth.
	expectCode(t, call(t, h, "GET", "/api/quizzes", "", ""), http.StatusUnauthorized)
	expectCode(t, call(t, h, "GET", "/api/quizzes", "abc", ""), http.StatusUnauthorized)
	expectCode(t, call(t, h, "POST", "/api/quizzes", "0", `{}`), http.StatusUnauthorized)

	// Create: defaults timeLimit to 20, keeps explicit values.
	body := fmt.Sprintf(`{"title":"  Planets  ","questions":[%s,{"text":"Hottest?","choices":["Venus","Mercury"],"correct":0,"timeLimit":30}]}`, goodQ)
	rec := call(t, h, "POST", "/api/quizzes", me, body)
	expectCode(t, rec, http.StatusCreated)
	if rec.Header().Get(common.HeaderTrace) == "" || !strings.Contains(rec.Header().Get(common.HeaderTrace), "postgres") {
		t.Errorf("X-Trace %q missing postgres", rec.Header().Get(common.HeaderTrace))
	}
	var q quiz
	decode(t, rec, &q)
	if q.ID == 0 || q.OwnerID != owner || q.Title != "Planets" || len(q.Questions) != 2 {
		t.Fatalf("bad created quiz: %+v", q)
	}
	if q.Questions[0].TimeLimit != 20 || q.Questions[1].TimeLimit != 30 || q.Questions[0].Correct != 1 {
		t.Fatalf("bad question fields: %+v", q.Questions)
	}
	id := fmt.Sprint(q.ID)

	// Validation: each case must be a 400 with a message.
	bad := []string{
		`{"title":"","questions":[` + goodQ + `]}`,
		`{"title":"` + strings.Repeat("x", 121) + `","questions":[` + goodQ + `]}`,
		`{"title":"T","questions":[]}`,
		`{"title":"T","questions":[` + strings.TrimSuffix(strings.Repeat(goodQ+",", 51), ",") + `]}`,
		`{"title":"T","questions":[{"text":"","choices":["a","b"],"correct":0}]}`,
		`{"title":"T","questions":[{"text":"a","choices":["x"],"correct":0}]}`,
		`{"title":"T","questions":[{"text":"a","choices":["a","b","c","d","e"],"correct":0}]}`,
		`{"title":"T","questions":[{"text":"a","choices":["a",""],"correct":0}]}`,
		`{"title":"T","questions":[{"text":"a","choices":["a","` + strings.Repeat("y", 81) + `"],"correct":0}]}`,
		`{"title":"T","questions":[{"text":"a","choices":["x","y"]}]}`,
		`{"title":"T","questions":[{"text":"a","choices":["x","y"],"correct":2}]}`,
		`{"title":"T","questions":[{"text":"a","choices":["x","y"],"correct":0,"timeLimit":4}]}`,
		`{"title":"T","questions":[{"text":"a","choices":["x","y"],"correct":0,"timeLimit":121}]}`,
		`{"title":"T","questions":[{"text":"` + strings.Repeat("q", 301) + `","choices":["x","y"],"correct":0}]}`,
		`{"title":`,
	}
	for i, b := range bad {
		rec := call(t, h, "POST", "/api/quizzes", me, b)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad case %d: status %d, want 400 (%s)", i, rec.Code, rec.Body.String())
		}
		var e map[string]string
		decode(t, rec, &e)
		if e["error"] == "" {
			t.Errorf("bad case %d: no error message", i)
		}
	}

	// List: own only, with question counts.
	expectCode(t, call(t, h, "POST", "/api/quizzes", them, fmt.Sprintf(`{"title":"Theirs","questions":[%s]}`, goodQ)), http.StatusCreated)
	var list struct {
		Quizzes []struct {
			ID            int64  `json:"id"`
			Title         string `json:"title"`
			QuestionCount int    `json:"questionCount"`
		} `json:"quizzes"`
	}
	rec = call(t, h, "GET", "/api/quizzes", me, "")
	expectCode(t, rec, http.StatusOK)
	decode(t, rec, &list)
	if len(list.Quizzes) != 1 || list.Quizzes[0].QuestionCount != 2 || list.Quizzes[0].Title != "Planets" {
		t.Fatalf("list: %s", rec.Body.String())
	}

	// Get: owner only.
	expectCode(t, call(t, h, "GET", "/api/quizzes/"+id, me, ""), http.StatusOK)
	expectCode(t, call(t, h, "GET", "/api/quizzes/"+id, them, ""), http.StatusNotFound)
	expectCode(t, call(t, h, "GET", "/api/quizzes/999999999999", me, ""), http.StatusNotFound)
	expectCode(t, call(t, h, "GET", "/api/quizzes/abc", me, ""), http.StatusNotFound)

	// Update: replaces questions, bumps updatedAt, owner only.
	put := `{"title":"Renamed","questions":[` + goodQ + `]}`
	expectCode(t, call(t, h, "PUT", "/api/quizzes/"+id, them, put), http.StatusNotFound)
	expectCode(t, call(t, h, "PUT", "/api/quizzes/"+id, me, `{"title":"","questions":[]}`), http.StatusBadRequest)
	rec = call(t, h, "PUT", "/api/quizzes/"+id, me, put)
	expectCode(t, rec, http.StatusOK)
	var upd quiz
	decode(t, rec, &upd)
	if upd.Title != "Renamed" || len(upd.Questions) != 1 || !upd.UpdatedAt.After(q.UpdatedAt) {
		t.Fatalf("update: %+v", upd)
	}
	var nq int
	db.QueryRow(`SELECT COUNT(*) FROM quiz.questions WHERE quiz_id = $1`, q.ID).Scan(&nq)
	if nq != 1 {
		t.Fatalf("questions in db = %d, want 1", nq)
	}

	// Internal fetch: any owner, includes correct answers, no auth needed.
	rec = call(t, h, "GET", "/internal/quizzes/"+id, "", "")
	expectCode(t, rec, http.StatusOK)
	var in quiz
	decode(t, rec, &in)
	if in.Title != "Renamed" || in.Questions[0].Correct != 1 {
		t.Fatalf("internal: %s", rec.Body.String())
	}
	expectCode(t, call(t, h, "GET", "/internal/quizzes/999999999999", "", ""), http.StatusNotFound)

	// Delete: owner only, then gone.
	expectCode(t, call(t, h, "DELETE", "/api/quizzes/"+id, them, ""), http.StatusNotFound)
	expectCode(t, call(t, h, "DELETE", "/api/quizzes/"+id, me, ""), http.StatusNoContent)
	expectCode(t, call(t, h, "GET", "/api/quizzes/"+id, me, ""), http.StatusNotFound)
	expectCode(t, call(t, h, "DELETE", "/api/quizzes/"+id, me, ""), http.StatusNotFound)
	rec = call(t, h, "GET", "/api/quizzes", me, "")
	if !strings.Contains(rec.Body.String(), `"quizzes":[]`) {
		t.Errorf("empty list body: %s", rec.Body.String())
	}

	// Health checks Postgres.
	expectCode(t, call(t, h, "GET", "/healthz", "", ""), http.StatusOK)
}

func TestSeedDemo(t *testing.T) {
	_, db := testHandler(t)
	owner := randOwner()
	t.Cleanup(func() { db.Exec(`DELETE FROM quiz.quizzes WHERE owner_id = $1`, owner) })
	ctx := context.Background()

	for range 2 { // second run must be a no-op
		if err := seedDemo(ctx, db, owner); err != nil {
			t.Fatal(err)
		}
	}
	var quizzes, questions int
	db.QueryRow(`SELECT COUNT(*) FROM quiz.quizzes WHERE owner_id = $1`, owner).Scan(&quizzes)
	db.QueryRow(`SELECT COUNT(*) FROM quiz.questions qs JOIN quiz.quizzes q ON q.id = qs.quiz_id WHERE q.owner_id = $1`, owner).Scan(&questions)
	if quizzes != 2 || questions != 16 {
		t.Fatalf("seeded %d quizzes, %d questions; want 2 and 16", quizzes, questions)
	}

	// Demo content must satisfy the same rules as user input.
	for _, d := range demoQuizzes {
		in := quizInput{Title: d.title}
		for _, qu := range d.questions {
			c := qu.Correct
			in.Questions = append(in.Questions, questionInput{Text: qu.Text, Choices: qu.Choices, Correct: &c, TimeLimit: &qu.TimeLimit})
		}
		if _, _, err := validate(in); err != nil {
			t.Errorf("demo %q: %v", d.title, err)
		}
	}
}
