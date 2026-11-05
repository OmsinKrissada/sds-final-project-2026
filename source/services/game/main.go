// Command game owns live Quizbox games: WebSocket hub, scoring, leaderboards
// and play history. State lives in Redis (fan-out via pub/sub) with durable
// snapshots in Postgres, so any number of replicas can serve any game.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"

	"github.com/lib/pq"
	"github.com/quizbox/quizbox/internal/common"
	"github.com/redis/go-redis/v9"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func isUniqueViolation(err error) bool {
	var pqe *pq.Error
	return errors.As(err, &pqe) && pqe.Code == "23505"
}

func main() {
	common.HandleNoop()

	db, err := sql.Open("postgres", common.Env("DATABASE_URL", "postgres://quizbox:quizbox@localhost:5432/quizbox?sslmode=disable"))
	if err != nil {
		slog.Error("postgres open", "err", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(5)

	rdb := redis.NewClient(&redis.Options{
		Addr:     common.Env("REDIS_ADDR", "localhost:6379"),
		Username: os.Getenv("REDIS_USERNAME"),
		Password: os.Getenv("REDIS_PASSWORD"),
	})

	hub := NewHub(rdb, db,
		common.NewClient(common.Env("QUIZ_URL", "http://localhost:8083")),
		common.NewClient(common.Env("ACCOUNT_URL", "http://localhost:8082")))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.RunSubscriber(ctx)

	common.OnShutdown(func() {
		cancel()
		hub.CloseAll(4503, "reconnect")
	})
	common.Run("game", hub.Routes())
}
