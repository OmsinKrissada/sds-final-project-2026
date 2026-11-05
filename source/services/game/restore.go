package main

// restore.go: lazy restore of a game from its Postgres snapshot after Redis lost it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/quizbox/quizbox/internal/common"
	"github.com/redis/go-redis/v9"
)

// ensureGame returns the game's meta hash, restoring Redis from Postgres when
// meta is gone but a non-finished session for the PIN was updated in the last 3h.
func (h *Hub) ensureGame(ctx context.Context, pin string) (map[string]string, error) {
	meta, err := h.rdb.HGetAll(ctx, kMeta(pin)).Result()
	common.Note(ctx, "redis")
	if err != nil {
		return nil, err
	}
	if len(meta) > 0 {
		return meta, nil
	}
	if h.db == nil {
		return nil, errNotFound
	}

	// Only one replica restores; the rest wait for meta (written last) to appear.
	lock := "game:" + pin + ":restore"
	got, err := h.rdb.SetNX(ctx, lock, "1", 15*time.Second).Result()
	if err != nil {
		return nil, err
	}
	if !got {
		for i := 0; i < 30; i++ {
			time.Sleep(100 * time.Millisecond)
			if meta, err = h.rdb.HGetAll(ctx, kMeta(pin)).Result(); err == nil && len(meta) > 0 {
				return meta, nil
			}
		}
		return nil, errNotFound
	}
	defer h.rdb.Del(context.WithoutCancel(ctx), lock)

	// Re-check under the lock: another replica may have just finished restoring.
	if meta, err = h.rdb.HGetAll(ctx, kMeta(pin)).Result(); err == nil && len(meta) > 0 {
		return meta, nil
	}
	var raw []byte
	err = h.db.QueryRowContext(ctx, `SELECT snapshot FROM game.sessions
WHERE pin=$1 AND status <> 'finished' AND updated_at > now() - interval '3 hours'
ORDER BY updated_at DESC LIMIT 1`, pin).Scan(&raw)
	common.Note(ctx, "postgres")
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	var s snapshot
	if err := json.Unmarshal(raw, &s); err != nil || len(s.Meta) == 0 {
		return nil, errNotFound // e.g. the '{}' placeholder from a half-created game
	}
	if err := h.restoreSnapshot(ctx, pin, &s); err != nil {
		return nil, err
	}
	logInfo("game restored from postgres", "pin", pin, "status", s.Meta["status"])
	h.publish(ctx, pin)
	return h.rdb.HGetAll(ctx, kMeta(pin)).Result()
}

// restoreSnapshot writes every key back; meta goes last so its presence means "complete".
func (h *Hub) restoreSnapshot(ctx context.Context, pin string, s *snapshot) error {
	ttl := gameTTL
	_, err := h.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, kQuestions(pin), s.Questions, ttl)
		for _, k := range []string{kPlayers(pin), kTokens(pin), kScores(pin)} {
			p.Del(ctx, k)
		}
		if len(s.Players) > 0 {
			p.HSet(ctx, kPlayers(pin), toArgs(s.Players)...)
			p.Expire(ctx, kPlayers(pin), ttl)
		}
		if len(s.Tokens) > 0 {
			p.HSet(ctx, kTokens(pin), toArgs(s.Tokens)...)
			p.Expire(ctx, kTokens(pin), ttl)
		}
		if len(s.Scores) > 0 {
			zs := make([]redis.Z, 0, len(s.Scores))
			for id, sc := range s.Scores {
				zs = append(zs, redis.Z{Score: sc, Member: id})
			}
			p.ZAdd(ctx, kScores(pin), zs...)
			p.Expire(ctx, kScores(pin), ttl)
		}
		for qi, ans := range s.Answers {
			i, _ := strconv.Atoi(qi)
			p.Del(ctx, kAns(pin, i))
			if len(ans) > 0 {
				p.HSet(ctx, kAns(pin, i), toArgs(ans)...)
				p.Expire(ctx, kAns(pin, i), ttl)
			}
		}
		p.HSet(ctx, kMeta(pin), toArgs(s.Meta)...)
		p.HIncrBy(ctx, kMeta(pin), "v", 1)
		p.Expire(ctx, kMeta(pin), ttl)
		return nil
	})
	common.Note(ctx, "redis")
	return err
}

func toArgs(m map[string]string) []any {
	out := make([]any, 0, len(m)*2)
	for k, v := range m {
		out = append(out, k, v)
	}
	return out
}
