package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"github.com/quizbox/quizbox/internal/common"
)

const sessionTTL = 7 * 24 * time.Hour

// User is the public user shape. Email is empty when the user was resolved
// from the Redis session cache alone (authz only needs id and name).
type User struct {
	ID          int64  `json:"id"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"displayName"`
}

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// store wraps Postgres (source of truth) and Redis (session cache).
type store struct {
	db  *sql.DB
	rdb *redis.Client
}

func sessionKey(hash string) string { return "sess:" + hash }

// tokenHash is the hex sha256 of a cookie token; only this is stored.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func isUniqueViolation(err error) bool {
	var pe *pq.Error
	return errors.As(err, &pe) && pe.Code == "23505"
}

// --- users (Postgres) ---

func (s *store) insertUser(ctx context.Context, q querier, email, name, pwHash string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx,
		`INSERT INTO account.users (email, display_name, pw_hash) VALUES ($1, $2, $3) RETURNING id`,
		email, name, pwHash).Scan(&id)
	common.Note(ctx, "postgres")
	return id, err
}

// userByEmail returns the id, name and password hash for email.
func (s *store) userByEmail(ctx context.Context, email string) (User, string, error) {
	var u User
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, display_name, pw_hash FROM account.users WHERE email = $1`, email).
		Scan(&u.ID, &u.DisplayName, &hash)
	common.Note(ctx, "postgres")
	u.Email = email
	return u, hash, err
}

func (s *store) userByID(ctx context.Context, id int64) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, display_name FROM account.users WHERE id = $1`, id).
		Scan(&u.ID, &u.Email, &u.DisplayName)
	common.Note(ctx, "postgres")
	return u, err
}

func (s *store) usersByIDs(ctx context.Context, ids []int64) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, display_name FROM account.users WHERE id = ANY($1::bigint[])`, pq.Array(ids))
	common.Note(ctx, "postgres")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.DisplayName); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ensureDemoUser creates the demo account if no user has its email.
func (s *store) ensureDemoUser(ctx context.Context, email, name, password string) error {
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM account.users WHERE email = $1)`, email).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	// The demo user gets id 1 so quiz's SEED_OWNER_ID=1 sample quizzes belong
	// to it (quiz cannot read account's schema). If id 1 is already taken by
	// someone else, fall back to a normal insert.
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO account.users (id, email, display_name, pw_hash) VALUES (1, $1, $2, $3) ON CONFLICT DO NOTHING`,
		email, name, hash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		_, err = s.db.ExecContext(ctx,
			`SELECT setval(pg_get_serial_sequence('account.users','id'), GREATEST((SELECT max(id) FROM account.users), 1))`)
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO account.users (email, display_name, pw_hash) VALUES ($1, $2, $3) ON CONFLICT (email) DO NOTHING`,
		email, name, hash)
	return err
}

// --- sessions ---

// issueSession inserts a new session row via q and returns the cookie token
// and the expiry. Callers cache it in Redis with cacheSession after commit.
func (s *store) issueSession(ctx context.Context, q querier, userID int64) (token, hash string, exp time.Time, err error) {
	if token, err = newToken(); err != nil {
		return "", "", time.Time{}, err
	}
	hash = tokenHash(token)
	exp = time.Now().Add(sessionTTL)
	_, err = q.ExecContext(ctx,
		`INSERT INTO account.sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		hash, userID, exp)
	common.Note(ctx, "postgres")
	return token, hash, exp, err
}

// cacheSession writes the session to Redis. Best-effort: failures are logged.
func (s *store) cacheSession(ctx context.Context, hash string, u User, exp time.Time) {
	ttl := time.Until(exp)
	if ttl <= 0 {
		return
	}
	key := sessionKey(hash)
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, key, "userId", u.ID, "name", u.DisplayName)
		p.Expire(ctx, key, ttl)
		return nil
	})
	if err != nil {
		slog.Warn("redis session cache write failed", "err", err)
		return
	}
	common.Note(ctx, "redis")
}

// resolveSession maps a cookie token to a user. Redis is tried first; on a
// miss or error Postgres is read (and Redis repopulated best-effort). ok is
// false for an unknown or expired session. Users resolved from Redis have no
// Email.
func (s *store) resolveSession(ctx context.Context, token string) (u User, ok bool, err error) {
	if token == "" {
		return User{}, false, nil
	}
	hash := tokenHash(token)

	if m, rerr := s.rdb.HGetAll(ctx, sessionKey(hash)).Result(); rerr == nil && len(m) > 0 {
		if id, perr := strconv.ParseInt(m["userId"], 10, 64); perr == nil {
			common.Note(ctx, "redis")
			return User{ID: id, DisplayName: m["name"]}, true, nil
		}
	} else if rerr != nil {
		slog.Warn("redis session read failed, falling back to postgres", "err", rerr)
	}

	var exp time.Time
	err = s.db.QueryRowContext(ctx,
		`SELECT u.id, u.email, u.display_name, s.expires_at
		   FROM account.sessions s JOIN account.users u ON u.id = s.user_id
		  WHERE s.token_hash = $1 AND s.expires_at > now()`, hash).
		Scan(&u.ID, &u.Email, &u.DisplayName, &exp)
	common.Note(ctx, "postgres")
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	s.cacheSession(ctx, hash, u, exp)
	return u, true, nil
}

// deleteSession removes the session from Postgres (authoritative) and Redis
// (best-effort).
func (s *store) deleteSession(ctx context.Context, token string) error {
	hash := tokenHash(token)
	_, err := s.db.ExecContext(ctx, `DELETE FROM account.sessions WHERE token_hash = $1`, hash)
	common.Note(ctx, "postgres")
	if err != nil {
		return err
	}
	if rerr := s.rdb.Del(ctx, sessionKey(hash)).Err(); rerr != nil {
		slog.Warn("redis session delete failed", "err", rerr)
		return nil
	}
	common.Note(ctx, "redis")
	return nil
}
