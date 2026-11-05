package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/quizbox/quizbox/internal/common"
)

type server struct{ db *sql.DB }

// quizInput is the request body for POST and PUT. Pointers distinguish
// "missing" from zero values for correct and timeLimit.
type quizInput struct {
	Title     string          `json:"title"`
	Questions []questionInput `json:"questions"`
}

type questionInput struct {
	Text      string   `json:"text"`
	Choices   []string `json:"choices"`
	Correct   *int     `json:"correct"`
	TimeLimit *int     `json:"timeLimit"`
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	owner, ok := ownerOf(w, r)
	if !ok {
		return
	}
	items, err := listQuizzes(r.Context(), s.db, owner)
	if err != nil {
		dbError(w, err)
		return
	}
	common.WriteJSON(w, http.StatusOK, map[string]any{"quizzes": items})
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	owner, ok := ownerOf(w, r)
	if !ok {
		return
	}
	title, qs, ok := readQuizBody(w, r)
	if !ok {
		return
	}
	q, err := createQuiz(r.Context(), s.db, owner, title, qs)
	if err != nil {
		dbError(w, err)
		return
	}
	common.WriteJSON(w, http.StatusCreated, q)
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	owner, ok := ownerOf(w, r)
	if !ok {
		return
	}
	id, ok := quizID(w, r)
	if !ok {
		return
	}
	q, err := loadQuiz(r.Context(), s.db, id, owner)
	if err != nil {
		loadError(w, err)
		return
	}
	common.WriteJSON(w, http.StatusOK, q)
}

func (s *server) update(w http.ResponseWriter, r *http.Request) {
	owner, ok := ownerOf(w, r)
	if !ok {
		return
	}
	id, ok := quizID(w, r)
	if !ok {
		return
	}
	title, qs, ok := readQuizBody(w, r)
	if !ok {
		return
	}
	q, err := updateQuiz(r.Context(), s.db, id, owner, title, qs)
	if err != nil {
		loadError(w, err)
		return
	}
	common.WriteJSON(w, http.StatusOK, q)
}

func (s *server) remove(w http.ResponseWriter, r *http.Request) {
	owner, ok := ownerOf(w, r)
	if !ok {
		return
	}
	id, ok := quizID(w, r)
	if !ok {
		return
	}
	found, err := deleteQuiz(r.Context(), s.db, id, owner)
	if err != nil {
		dbError(w, err)
		return
	}
	if !found {
		common.WriteError(w, http.StatusNotFound, "quiz not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// internalGet returns any owner's quiz, including correct answers. Used by game only.
func (s *server) internalGet(w http.ResponseWriter, r *http.Request) {
	id, ok := quizID(w, r)
	if !ok {
		return
	}
	q, err := loadQuiz(r.Context(), s.db, id, 0)
	if err != nil {
		loadError(w, err)
		return
	}
	common.WriteJSON(w, http.StatusOK, q)
}

// ownerOf reads the gateway-set X-User-Id header, answering 401 when absent or invalid.
func ownerOf(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.Header.Get(common.HeaderUserID), 10, 64)
	if err != nil || id <= 0 {
		common.WriteError(w, http.StatusUnauthorized, "login required")
		return 0, false
	}
	return id, true
}

func quizID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		common.WriteError(w, http.StatusNotFound, "quiz not found")
		return 0, false
	}
	return id, true
}

func readQuizBody(w http.ResponseWriter, r *http.Request) (string, []question, bool) {
	var in quizInput
	if err := common.ReadJSON(r, &in); err != nil {
		common.WriteError(w, http.StatusBadRequest, "body must be JSON {title, questions}")
		return "", nil, false
	}
	title, qs, err := validate(in)
	if err != nil {
		common.WriteError(w, http.StatusBadRequest, err.Error())
		return "", nil, false
	}
	return title, qs, true
}

// validate applies the contract limits and returns trimmed, normalized values.
func validate(in quizInput) (string, []question, error) {
	title := strings.TrimSpace(in.Title)
	if n := utf8.RuneCountInString(title); n < 1 || n > 120 {
		return "", nil, errors.New("title must be 1-120 characters")
	}
	if n := len(in.Questions); n < 1 || n > 50 {
		return "", nil, errors.New("questions must contain 1-50 items")
	}
	qs := make([]question, len(in.Questions))
	for i, qi := range in.Questions {
		p := fmt.Sprintf("questions[%d]", i)
		text := strings.TrimSpace(qi.Text)
		if n := utf8.RuneCountInString(text); n < 1 || n > 300 {
			return "", nil, fmt.Errorf("%s.text must be 1-300 characters", p)
		}
		if n := len(qi.Choices); n < 2 || n > 4 {
			return "", nil, fmt.Errorf("%s.choices must have 2-4 entries", p)
		}
		choices := make([]string, len(qi.Choices))
		for j, c := range qi.Choices {
			c = strings.TrimSpace(c)
			if n := utf8.RuneCountInString(c); n < 1 || n > 80 {
				return "", nil, fmt.Errorf("%s.choices[%d] must be 1-80 characters", p, j)
			}
			choices[j] = c
		}
		if qi.Correct == nil {
			return "", nil, fmt.Errorf("%s.correct is required", p)
		}
		if *qi.Correct < 0 || *qi.Correct >= len(choices) {
			return "", nil, fmt.Errorf("%s.correct must be a choice index 0-%d", p, len(choices)-1)
		}
		tl := 20
		if qi.TimeLimit != nil {
			tl = *qi.TimeLimit
		}
		if tl < 5 || tl > 120 {
			return "", nil, fmt.Errorf("%s.timeLimit must be 5-120 seconds", p)
		}
		qs[i] = question{Text: text, Choices: choices, Correct: *qi.Correct, TimeLimit: tl}
	}
	return title, qs, nil
}

// loadError maps a store error to 404 when the row is absent, else 500.
func loadError(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		common.WriteError(w, http.StatusNotFound, "quiz not found")
		return
	}
	dbError(w, err)
}

func dbError(w http.ResponseWriter, err error) {
	slog.Error("quiz db", "err", err)
	common.WriteError(w, http.StatusInternalServerError, "database error")
}
