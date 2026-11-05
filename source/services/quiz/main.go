// Command quiz serves quiz CRUD for hosts (/api/quizzes) and a read-only
// internal lookup used by the game service (/internal/quizzes/{id}).
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"

	_ "github.com/lib/pq"

	"github.com/quizbox/quizbox/internal/common"
)

const defaultDSN = "postgres://quizbox:quizbox@localhost:5432/quizbox?sslmode=disable"

func main() {
	common.HandleNoop()
	db, err := openDB(common.Env("DATABASE_URL", defaultDSN))
	if err != nil {
		slog.Error("open postgres", "err", err)
		os.Exit(1)
	}
	if os.Getenv("SEED_DEMO") == "true" {
		owner := int64(common.EnvInt("SEED_OWNER_ID", 1))
		if err := seedDemo(context.Background(), db, owner); err != nil {
			slog.Error("seed demo quizzes", "err", err)
		}
	}
	common.Run("quiz", newMux(db))
}

func openDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	return db, nil
}

func newMux(db *sql.DB) *http.ServeMux {
	s := &server{db: db}
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", common.Health(map[string]common.Check{
		"postgres": func(ctx context.Context) error { return db.PingContext(ctx) },
	}))
	mux.HandleFunc("GET /api/quizzes", s.list)
	mux.HandleFunc("POST /api/quizzes", s.create)
	mux.HandleFunc("GET /api/quizzes/{id}", s.get)
	mux.HandleFunc("PUT /api/quizzes/{id}", s.update)
	mux.HandleFunc("DELETE /api/quizzes/{id}", s.remove)
	mux.HandleFunc("GET /internal/quizzes/{id}", s.internalGet)
	return mux
}
