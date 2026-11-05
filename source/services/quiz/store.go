package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/quizbox/quizbox/internal/common"
)

// Quiz is the JSON shape returned by every quiz endpoint.
type quiz struct {
	ID        int64      `json:"id"`
	OwnerID   int64      `json:"ownerId"`
	Title     string     `json:"title"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	Questions []question `json:"questions"`
}

type question struct {
	Text      string   `json:"text"`
	Choices   []string `json:"choices"`
	Correct   int      `json:"correct"`
	TimeLimit int      `json:"timeLimit"`
}

type listItem struct {
	ID            int64     `json:"id"`
	Title         string    `json:"title"`
	QuestionCount int       `json:"questionCount"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func listQuizzes(ctx context.Context, db *sql.DB, owner int64) ([]listItem, error) {
	common.Note(ctx, "postgres")
	rows, err := db.QueryContext(ctx, `
		SELECT q.id, q.title, COUNT(qs.id), q.updated_at
		FROM quiz.quizzes q LEFT JOIN quiz.questions qs ON qs.quiz_id = q.id
		WHERE q.owner_id = $1
		GROUP BY q.id
		ORDER BY q.created_at DESC, q.id DESC`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []listItem{}
	for rows.Next() {
		var it listItem
		if err := rows.Scan(&it.ID, &it.Title, &it.QuestionCount, &it.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// loadQuiz fetches one quiz. owner 0 means any owner (internal use).
// Returns sql.ErrNoRows when absent or not owned.
func loadQuiz(ctx context.Context, db *sql.DB, id, owner int64) (*quiz, error) {
	common.Note(ctx, "postgres")
	var q quiz
	err := db.QueryRowContext(ctx, `
		SELECT id, owner_id, title, created_at, updated_at
		FROM quiz.quizzes
		WHERE id = $1 AND ($2::bigint = 0 OR owner_id = $2::bigint)`, id, owner).
		Scan(&q.ID, &q.OwnerID, &q.Title, &q.CreatedAt, &q.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if q.Questions, err = loadQuestions(ctx, db, id); err != nil {
		return nil, err
	}
	return &q, nil
}

func loadQuestions(ctx context.Context, q querier, quizID int64) ([]question, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT text, choices, correct, time_limit_s
		FROM quiz.questions WHERE quiz_id = $1 ORDER BY idx`, quizID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []question{}
	for rows.Next() {
		var qu question
		var choices []byte
		if err := rows.Scan(&qu.Text, &choices, &qu.Correct, &qu.TimeLimit); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(choices, &qu.Choices); err != nil {
			return nil, err
		}
		out = append(out, qu)
	}
	return out, rows.Err()
}

func createQuiz(ctx context.Context, db *sql.DB, owner int64, title string, qs []question) (*quiz, error) {
	common.Note(ctx, "postgres")
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q, err := insertQuiz(ctx, tx, owner, title, qs)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return q, nil
}

// insertQuiz writes a quiz row and its questions using q (a tx or the pool).
func insertQuiz(ctx context.Context, q querier, owner int64, title string, qs []question) (*quiz, error) {
	out := quiz{OwnerID: owner, Title: title, Questions: qs}
	err := q.QueryRowContext(ctx, `
		INSERT INTO quiz.quizzes (owner_id, title) VALUES ($1, $2)
		RETURNING id, created_at, updated_at`, owner, title).
		Scan(&out.ID, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err := insertQuestions(ctx, q, out.ID, qs); err != nil {
		return nil, err
	}
	return &out, nil
}

// updateQuiz replaces title and questions for an owned quiz. Returns
// sql.ErrNoRows when the quiz is absent or owned by someone else.
func updateQuiz(ctx context.Context, db *sql.DB, id, owner int64, title string, qs []question) (*quiz, error) {
	common.Note(ctx, "postgres")
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := quiz{ID: id, OwnerID: owner, Title: title, Questions: qs}
	err = tx.QueryRowContext(ctx, `
		UPDATE quiz.quizzes SET title = $3, updated_at = now()
		WHERE id = $1 AND owner_id = $2
		RETURNING created_at, updated_at`, id, owner, title).
		Scan(&out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM quiz.questions WHERE quiz_id = $1`, id); err != nil {
		return nil, err
	}
	if err := insertQuestions(ctx, tx, id, qs); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &out, nil
}

func insertQuestions(ctx context.Context, q querier, quizID int64, qs []question) error {
	for i, qu := range qs {
		choices, err := json.Marshal(qu.Choices)
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `
			INSERT INTO quiz.questions (quiz_id, idx, text, choices, correct, time_limit_s)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			quizID, i, qu.Text, string(choices), qu.Correct, qu.TimeLimit); err != nil {
			return err
		}
	}
	return nil
}

func deleteQuiz(ctx context.Context, db *sql.DB, id, owner int64) (bool, error) {
	common.Note(ctx, "postgres")
	res, err := db.ExecContext(ctx, `DELETE FROM quiz.quizzes WHERE id = $1 AND owner_id = $2`, id, owner)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
