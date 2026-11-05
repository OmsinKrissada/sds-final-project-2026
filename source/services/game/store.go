package main

// store.go: Postgres persistence (game.sessions snapshots, game.results, history)
// and the all-time leaderboard rebuild.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/lib/pq"
	"github.com/quizbox/quizbox/internal/common"
	"github.com/redis/go-redis/v9"
)

// snapshot is the JSON stored in game.sessions.snapshot: every Redis key of a game.
type snapshot struct {
	Pin       string                       `json:"pin"`
	Meta      map[string]string            `json:"meta"`
	Questions string                       `json:"questions"`
	Players   map[string]string            `json:"players"`
	Tokens    map[string]string            `json:"tokens"`
	Answers   map[string]map[string]string `json:"answers"` // question index -> playerId -> JSON
	Scores    map[string]float64           `json:"scores"`
}

// takeSnapshot reads all of a game's keys from Redis.
func (h *Hub) takeSnapshot(ctx context.Context, pin string) (*snapshot, error) {
	pipe := h.rdb.Pipeline()
	mc := pipe.HGetAll(ctx, kMeta(pin))
	qc := pipe.Get(ctx, kQuestions(pin))
	pc := pipe.HGetAll(ctx, kPlayers(pin))
	tc := pipe.HGetAll(ctx, kTokens(pin))
	sc := pipe.ZRangeWithScores(ctx, kScores(pin), 0, -1)
	_, err := pipe.Exec(ctx)
	common.Note(ctx, "redis")
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	if len(mc.Val()) == 0 {
		return nil, errNotFound
	}
	s := &snapshot{Pin: pin, Meta: mc.Val(), Questions: qc.Val(), Players: pc.Val(), Tokens: tc.Val(),
		Answers: map[string]map[string]string{}, Scores: map[string]float64{}}
	for _, z := range sc.Val() {
		s.Scores[z.Member.(string)] = z.Score
	}
	var qs []Question
	_ = json.Unmarshal([]byte(s.Questions), &qs)
	pipe = h.rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(qs))
	for i := range qs {
		cmds[i] = pipe.HGetAll(ctx, kAns(pin, i))
	}
	if len(qs) > 0 {
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return nil, err
		}
	}
	for i, c := range cmds {
		if len(c.Val()) > 0 {
			s.Answers[strconv.Itoa(i)] = c.Val()
		}
	}
	return s, nil
}

const upsertSQL = `
INSERT INTO game.sessions (id, pin, quiz_id, host_id, status, current_q, snapshot, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7, now())
ON CONFLICT (id) DO UPDATE
   SET status = EXCLUDED.status, current_q = EXCLUDED.current_q,
       snapshot = EXCLUDED.snapshot, updated_at = now()
 WHERE game.sessions.status <> 'finished'
   AND COALESCE((game.sessions.snapshot->'meta'->>'v')::bigint, 0) <= $8
RETURNING id`

// persist upserts game.sessions with a fresh snapshot (called on every status
// transition). A stale writer can never overwrite a newer v or a finished row.
func (h *Hub) persist(ctx context.Context, pin string) {
	if h.db == nil {
		return
	}
	snap, err := h.takeSnapshot(ctx, pin)
	if err != nil {
		logWarn("snapshot failed", "pin", pin, "err", err)
		return
	}
	if err := h.upsert(ctx, h.db, snap); err != nil && !errors.Is(err, sql.ErrNoRows) {
		logWarn("persist failed", "pin", pin, "err", err)
	}
}

type execQuerier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func (h *Hub) upsert(ctx context.Context, db execQuerier, s *snapshot) error {
	raw, _ := json.Marshal(s)
	m := s.Meta
	sid, _ := strconv.ParseInt(m["sessionId"], 10, 64)
	quizID, _ := strconv.ParseInt(m["quizId"], 10, 64)
	hostID, _ := strconv.ParseInt(m["hostId"], 10, 64)
	q, _ := strconv.Atoi(m["q"])
	v, _ := strconv.ParseInt(m["v"], 10, 64)
	var id int64
	err := db.QueryRowContext(ctx, upsertSQL, sid, s.Pin, quizID, hostID, m["status"], q, raw, v).Scan(&id)
	common.Note(ctx, "postgres")
	return err
}

// finishGame is run once by whichever replica won the ->finished transition.
// It writes game.results and bumps lb:alltime exactly once per session: the
// session row flips to finished inside the same transaction as the results.
func (h *Hub) finishGame(ctx context.Context, pin string) {
	if h.db == nil {
		return
	}
	snap, err := h.takeSnapshot(ctx, pin)
	if err != nil {
		logWarn("finish snapshot failed", "pin", pin, "err", err)
		return
	}
	g, err := h.loadGame(ctx, pin)
	if err != nil {
		logWarn("finish load failed", "pin", pin, "err", err)
		return
	}
	h.ensureLeaderboard(ctx) // before results exist, so the rebuild can't double count

	snap.Meta["status"] = "finished"
	raw, _ := json.Marshal(snap)
	m := snap.Meta
	sid, _ := strconv.ParseInt(m["sessionId"], 10, 64)
	quizID, _ := strconv.ParseInt(m["quizId"], 10, 64)
	hostID, _ := strconv.ParseInt(m["hostId"], 10, 64)
	q, _ := strconv.Atoi(m["q"])

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		logWarn("finish tx failed", "pin", pin, "err", err)
		return
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO game.sessions (id, pin, quiz_id, host_id, status, current_q, snapshot, updated_at)
VALUES ($1,$2,$3,$4,'finished',$5,$6, now())
ON CONFLICT (id) DO UPDATE SET status='finished', current_q=EXCLUDED.current_q, snapshot=EXCLUDED.snapshot, updated_at=now()
 WHERE game.sessions.status <> 'finished'
RETURNING id`, sid, pin, quizID, hostID, q, raw).Scan(&id)
	common.Note(ctx, "postgres")
	if errors.Is(err, sql.ErrNoRows) {
		return // already finished by someone else
	}
	if err != nil {
		logWarn("finish upsert failed", "pin", pin, "err", err)
		return
	}
	ranked := g.finalRanking()
	for _, r := range ranked {
		var uid any
		if r.userID != nil {
			uid = *r.userID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO game.results (session_id,pin,quiz_title,player_id,nick,score,rank)
VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, pin, g.Meta["quizTitle"], uid, r.nick, r.score, r.rank); err != nil {
			logWarn("finish result failed", "pin", pin, "err", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		logWarn("finish commit failed", "pin", pin, "err", err)
		return
	}
	pipe := h.rdb.Pipeline()
	for _, r := range ranked {
		if r.userID != nil {
			pipe.ZIncrBy(ctx, "lb:alltime", float64(r.score), strconv.FormatInt(*r.userID, 10))
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		logWarn("lb update failed", "pin", pin, "err", err)
	}
	common.Note(ctx, "redis")
}

type finalRow struct {
	nick   string
	userID *int64
	score  int
	rank   int
}

// finalRanking ranks players by score desc (ties share a rank: 1,1,3).
func (g *gameData) finalRanking() []finalRow {
	ranked := g.ranked() // status is finished here, so nothing is hidden
	out := make([]finalRow, len(ranked))
	for i, r := range ranked {
		rank := i + 1
		if i > 0 && r.Score == ranked[i-1].Score {
			rank = out[i-1].rank
		}
		out[i] = finalRow{nick: r.Nick, userID: g.Players[r.ID].UserID, score: r.Score, rank: rank}
	}
	return out
}

// ensureLeaderboard rebuilds lb:alltime from game.results when the "built" flag is gone.
func (h *Hub) ensureLeaderboard(ctx context.Context) {
	if h.db == nil {
		return
	}
	n, err := h.rdb.Exists(ctx, "lb:alltime:built").Result()
	common.Note(ctx, "redis")
	if err != nil || n > 0 {
		return
	}
	// one replica rebuilds; others serve what they have
	if ok, _ := h.rdb.SetNX(ctx, "lb:alltime:rebuild", "1", 30*time.Second).Result(); !ok {
		return
	}
	defer h.rdb.Del(ctx, "lb:alltime:rebuild")
	rows, err := h.db.QueryContext(ctx, `SELECT player_id, SUM(score) FROM game.results WHERE player_id IS NOT NULL GROUP BY player_id`)
	common.Note(ctx, "postgres")
	if err != nil {
		logWarn("lb rebuild query failed", "err", err)
		return
	}
	defer rows.Close()
	var zs []redis.Z
	for rows.Next() {
		var id, sum int64
		if rows.Scan(&id, &sum) == nil {
			zs = append(zs, redis.Z{Score: float64(sum), Member: strconv.FormatInt(id, 10)})
		}
	}
	if rows.Err() != nil {
		return
	}
	_, err = h.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Del(ctx, "lb:alltime")
		if len(zs) > 0 {
			p.ZAdd(ctx, "lb:alltime", zs...)
		}
		p.Set(ctx, "lb:alltime:built", "1", 0)
		return nil
	})
	common.Note(ctx, "redis")
	if err != nil {
		logWarn("lb rebuild failed", "err", err)
	}
}

// insertSession reserves a PIN in Postgres. A unique violation means the PIN
// is live (or a stale unfinished row older than the Redis TTL, which we retire).
func (h *Hub) insertSession(ctx context.Context, pin string, quizID, hostID int64) (int64, error) {
	for attempt := 0; attempt < 2; attempt++ {
		var id int64
		err := h.db.QueryRowContext(ctx, `INSERT INTO game.sessions (pin, quiz_id, host_id, status, current_q, snapshot)
VALUES ($1,$2,$3,'lobby',-1,'{}') RETURNING id`, pin, quizID, hostID).Scan(&id)
		common.Note(ctx, "postgres")
		var pqe *pq.Error
		if errors.As(err, &pqe) && pqe.Code == "23505" && attempt == 0 {
			res, _ := h.db.ExecContext(ctx, `UPDATE game.sessions SET status='finished'
WHERE pin=$1 AND status <> 'finished' AND updated_at < now() - interval '3 hours'`, pin)
			if n, _ := res.RowsAffected(); n > 0 {
				continue
			}
		}
		return id, err
	}
	return 0, errConflict
}

func (h *Hub) deleteSession(ctx context.Context, id int64) {
	_, _ = h.db.ExecContext(ctx, `DELETE FROM game.sessions WHERE id=$1`, id)
}
