// Command account serves registration, login, sessions and user lookups.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"github.com/quizbox/quizbox/internal/common"
)

const demoEmail, demoPassword, demoName = "demo@quizbox.local", "demo1234", "Demo Host"

func main() {
	common.HandleNoop()

	db, err := sql.Open("postgres", common.Env("DATABASE_URL",
		"postgres://quizbox:quizbox@localhost:5432/quizbox?sslmode=disable"))
	if err != nil {
		slog.Error("open postgres", "err", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(5)

	// go-redis connects lazily, so a Redis outage at startup is not fatal:
	// sessions fall back to Postgres. Short timeouts keep fallback fast.
	rdb := redis.NewClient(&redis.Options{
		Addr:         common.Env("REDIS_ADDR", "localhost:6379"),
		Username:     common.Env("REDIS_USERNAME", ""),
		Password:     common.Env("REDIS_PASSWORD", ""),
		DialTimeout:  500 * time.Millisecond,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
		MaxRetries:   -1,
	})

	st := &store{db: db, rdb: rdb}
	if common.Env("SEED_DEMO", "false") == "true" {
		go seedDemo(st)
	}

	srv := &server{st: st, cookieSecure: common.Env("COOKIE_SECURE", "false") == "true"}
	mux := http.NewServeMux()
	srv.routes(mux)
	// Readiness depends on Postgres only; Redis is a cache with a PG fallback.
	mux.Handle("GET /healthz", common.Health(map[string]common.Check{
		"postgres": func(ctx context.Context) error { return db.PingContext(ctx) },
	}))

	common.Run("account", mux)
}

// seedDemo retries until Postgres accepts the demo user, so startup does not
// block on a database that is still coming up.
func seedDemo(st *store) {
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := st.ensureDemoUser(ctx, demoEmail, demoName, demoPassword)
		cancel()
		if err == nil {
			slog.Info("demo user ready", "email", demoEmail)
			return
		}
		slog.Warn("seed demo user failed, retrying", "attempt", attempt, "err", err)
		time.Sleep(5 * time.Second)
	}
}
