package main

// state.go: Redis layout, atomic Lua transitions and the per-viewer state snapshot.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/quizbox/quizbox/internal/common"
	"github.com/redis/go-redis/v9"
)

const (
	eventsChannel = "game:events"
	gameTTL       = 3 * time.Hour
	maxPlayers    = 100
	answerGraceMs = 500
)

var (
	errNotFound = errors.New("game not found")
	errConflict = errors.New("conflict")
)

// Redis key helpers.
func kMeta(pin string) string      { return "game:" + pin + ":meta" }
func kQuestions(pin string) string { return "game:" + pin + ":questions" }
func kPlayers(pin string) string   { return "game:" + pin + ":players" }
func kTokens(pin string) string    { return "game:" + pin + ":tokens" }
func kScores(pin string) string    { return "game:" + pin + ":scores" }
func kAns(pin string, q int) string {
	return "game:" + pin + ":q:" + strconv.Itoa(q) + ":ans"
}

// Question is stored in game:{pin}:questions (with the correct answer).
type Question struct {
	Text      string   `json:"text"`
	Choices   []string `json:"choices"`
	Correct   int      `json:"correct"`
	TimeLimit int      `json:"timeLimit"`
}

type playerRec struct {
	Nick   string `json:"nick"`
	UserID *int64 `json:"userId"`
}

type answerRec struct {
	Choice int   `json:"choice"`
	MS     int64 `json:"ms"`
	Points int   `json:"points"`
}

// ---- Lua scripts: every state mutation is atomic on the Redis side. -------

// transitionScript moves meta to a new status only if the current status (and
// optionally q) is what the caller expected. Returns the new v, -1 if the game
// is missing, -2 if the expectation failed (double click, other replica won).
var transitionScript = redis.NewScript(`
local m = redis.call('HMGET', KEYS[1], 'status', 'q')
if not m[1] then return -1 end
if not string.find(',' .. ARGV[1] .. ',', ',' .. m[1] .. ',', 1, true) then return -2 end
if ARGV[2] ~= '' and m[2] ~= ARGV[2] then return -2 end
redis.call('HSET', KEYS[1], 'status', ARGV[3], 'q', ARGV[4], 'qStart', ARGV[5], 'deadline', ARGV[6])
redis.call('EXPIRE', KEYS[1], ARGV[7])
return redis.call('HINCRBY', KEYS[1], 'v', 1)
`)

// answerScript records one answer per player for the current question and adds
// points. Returns new v, 0 if the player already answered, -1 if the question
// is no longer the live one.
var answerScript = redis.NewScript(`
local m = redis.call('HMGET', KEYS[1], 'status', 'q', 'qStart')
if m[1] ~= 'question' or m[2] ~= ARGV[1] or m[3] ~= ARGV[2] then return -1 end
if redis.call('HSETNX', KEYS[2], ARGV[3], ARGV[4]) == 0 then return 0 end
redis.call('ZINCRBY', KEYS[3], ARGV[5], ARGV[3])
redis.call('EXPIRE', KEYS[2], ARGV[6])
redis.call('EXPIRE', KEYS[3], ARGV[6])
redis.call('EXPIRE', KEYS[1], ARGV[6])
return redis.call('HINCRBY', KEYS[1], 'v', 1)
`)

// createScript claims a PIN: returns 0 if meta already exists.
var createScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return 0 end
redis.call('SET', KEYS[2], ARGV[1], 'EX', ARGV[2])
local f = {}
for i = 3, #ARGV do f[#f + 1] = ARGV[i] end
redis.call('HSET', KEYS[1], unpack(f))
redis.call('EXPIRE', KEYS[1], ARGV[2])
return 1
`)

func ttlSecs() int { return int(gameTTL / time.Second) }

// touch refreshes the TTL of every key of a game.
func (h *Hub) touch(ctx context.Context, pin string, qCount int) {
	pipe := h.rdb.Pipeline()
	for _, k := range []string{kMeta(pin), kQuestions(pin), kPlayers(pin), kTokens(pin), kScores(pin)} {
		pipe.Expire(ctx, k, gameTTL)
	}
	for i := 0; i < qCount; i++ {
		pipe.Expire(ctx, kAns(pin, i), gameTTL)
	}
	_, _ = pipe.Exec(ctx)
	common.Note(ctx, "redis")
}

// publish tells every replica (including this one) that pin changed.
func (h *Hub) publish(ctx context.Context, pin string) {
	if err := h.rdb.Publish(ctx, eventsChannel, pin).Err(); err != nil {
		logWarn("publish failed", "pin", pin, "err", err)
	}
	common.Note(ctx, "redis")
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func randPlayerID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "p" + strconv.FormatUint(uint64(b[0])<<24|uint64(b[1])<<16|uint64(b[2])<<8|uint64(b[3]), 36)
}

func randPIN() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	n := (uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])) % 900000
	return fmt.Sprintf("%06d", n+100000)
}

// ---- loading ---------------------------------------------------------------

// gameData is one consistent-enough read of a game's Redis state.
type gameData struct {
	Pin       string
	Meta      map[string]string
	Questions []Question
	Players   map[string]playerRec
	Scores    map[string]float64
	Answers   map[string]answerRec // answers for the current question
}

func (g *gameData) status() string { return g.Meta["status"] }
func (g *gameData) q() int         { n, _ := strconv.Atoi(g.Meta["q"]); return n }
func (g *gameData) v() int64       { n, _ := strconv.ParseInt(g.Meta["v"], 10, 64); return n }
func (g *gameData) deadline() int64 {
	n, _ := strconv.ParseInt(g.Meta["deadline"], 10, 64)
	return n
}
func (g *gameData) qStart() int64 { n, _ := strconv.ParseInt(g.Meta["qStart"], 10, 64); return n }

// loadGame reads the whole game (restoring it from Postgres when Redis lost it).
func (h *Hub) loadGame(ctx context.Context, pin string) (*gameData, error) {
	meta, err := h.ensureGame(ctx, pin)
	if err != nil {
		return nil, err
	}
	g := &gameData{Pin: pin, Meta: meta, Players: map[string]playerRec{}, Scores: map[string]float64{}, Answers: map[string]answerRec{}}
	q := g.q()
	pipe := h.rdb.Pipeline()
	qc := pipe.Get(ctx, kQuestions(pin))
	pc := pipe.HGetAll(ctx, kPlayers(pin))
	sc := pipe.ZRangeWithScores(ctx, kScores(pin), 0, -1)
	var ac *redis.MapStringStringCmd
	if q >= 0 {
		ac = pipe.HGetAll(ctx, kAns(pin, q))
	}
	_, err = pipe.Exec(ctx)
	common.Note(ctx, "redis")
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	if raw, e := qc.Result(); e == nil {
		_ = json.Unmarshal([]byte(raw), &g.Questions)
	}
	for id, raw := range pc.Val() {
		var p playerRec
		if json.Unmarshal([]byte(raw), &p) == nil {
			g.Players[id] = p
		}
	}
	for _, z := range sc.Val() {
		g.Scores[fmt.Sprint(z.Member)] = z.Score
	}
	if ac != nil {
		for id, raw := range ac.Val() {
			var a answerRec
			if json.Unmarshal([]byte(raw), &a) == nil {
				g.Answers[id] = a
			}
		}
	}
	return g, nil
}

// ---- player join -------------------------------------------------------------

func trimNick(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		r = r[:max]
	}
	return string(r)
}

// uniqueNick appends a digit (2, 3, ...) until the nick is free.
func uniqueNick(nick string, taken map[string]bool) string {
	if !taken[strings.ToLower(nick)] {
		return nick
	}
	for n := 2; ; n++ {
		sfx := strconv.Itoa(n)
		c := trimNick(nick, 20-len(sfx)) + sfx
		if !taken[strings.ToLower(c)] {
			return c
		}
	}
}

var (
	errFull     = errors.New("game is full")
	errFinished = errors.New("game already finished")
)

// join registers a new player (WATCH on players makes nick uniqueness and the
// 100 player cap safe across replicas).
func (h *Hub) join(ctx context.Context, pin, nick string, userID *int64) (playerID, token string, err error) {
	playerID, token = randPlayerID(), randToken(24)
	for attempt := 0; attempt < 8; attempt++ {
		err = h.rdb.Watch(ctx, func(tx *redis.Tx) error {
			status, err := tx.HGet(ctx, kMeta(pin), "status").Result()
			if errors.Is(err, redis.Nil) {
				return errNotFound
			} else if err != nil {
				return err
			}
			if status == "finished" {
				return errFinished
			}
			all, err := tx.HGetAll(ctx, kPlayers(pin)).Result()
			if err != nil {
				return err
			}
			if len(all) >= maxPlayers {
				return errFull
			}
			taken := map[string]bool{}
			for _, raw := range all {
				var p playerRec
				if json.Unmarshal([]byte(raw), &p) == nil {
					taken[strings.ToLower(p.Nick)] = true
				}
			}
			rec, _ := json.Marshal(playerRec{Nick: uniqueNick(nick, taken), UserID: userID})
			_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
				p.HSet(ctx, kPlayers(pin), playerID, string(rec))
				p.HSet(ctx, kTokens(pin), token, playerID)
				p.ZAddNX(ctx, kScores(pin), redis.Z{Score: 0, Member: playerID})
				p.HIncrBy(ctx, kMeta(pin), "v", 1)
				p.Expire(ctx, kPlayers(pin), gameTTL)
				p.Expire(ctx, kTokens(pin), gameTTL)
				p.Expire(ctx, kScores(pin), gameTTL)
				return nil
			})
			return err
		}, kPlayers(pin))
		common.Note(ctx, "redis")
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		break
	}
	if err != nil {
		return "", "", err
	}
	h.publish(ctx, pin)
	return playerID, token, nil
}

// ---- state snapshot sent to sockets -------------------------------------------

type stateQuestion struct {
	Text      string   `json:"text"`
	Choices   []string `json:"choices"`
	TimeLimit int      `json:"timeLimit"`
	Deadline  int64    `json:"deadline"`
	ServerNow int64    `json:"serverNow"`
}
type stateReveal struct {
	Correct int   `json:"correct"`
	Counts  []int `json:"counts"`
}
type statePlayer struct {
	ID   string `json:"id"`
	Nick string `json:"nick"`
}
type stateScore struct {
	ID    string `json:"id"`
	Nick  string `json:"nick"`
	Score int    `json:"score"`
}
type stateYou struct {
	ID          string `json:"id"`
	Nick        string `json:"nick"`
	Score       int    `json:"score"`
	Answered    bool   `json:"answered"`
	LastPoints  *int   `json:"lastPoints,omitempty"`
	LastCorrect *bool  `json:"lastCorrect,omitempty"`
}
type stateGame struct {
	Pin         string         `json:"pin"`
	Status      string         `json:"status"`
	QuizTitle   string         `json:"quizTitle"`
	QIndex      int            `json:"qIndex"`
	QCount      int            `json:"qCount"`
	Question    *stateQuestion `json:"question,omitempty"`
	Answered    int            `json:"answered"`
	PlayerCount int            `json:"playerCount"`
	Players     []statePlayer  `json:"players"`
	Reveal      *stateReveal   `json:"reveal,omitempty"`
	Scoreboard  []stateScore   `json:"scoreboard"`
	You         *stateYou      `json:"you,omitempty"`
}
type stateMsg struct {
	Type string    `json:"type"`
	V    int64     `json:"v"`
	Game stateGame `json:"game"`
}

// ranked returns players sorted by score desc (ties by nick), with scores as
// the viewers may see them: while a question is open the points of the open
// question are withheld, otherwise the score jump would reveal who was right.
func (g *gameData) ranked() []stateScore {
	hide := g.status() == "question"
	out := make([]stateScore, 0, len(g.Players))
	for id, p := range g.Players {
		s := int(math.Round(g.Scores[id]))
		if hide {
			s -= g.Answers[id].Points
		}
		out = append(out, stateScore{ID: id, Nick: p.Nick, Score: s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if a, b := strings.ToLower(out[i].Nick), strings.ToLower(out[j].Nick); a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// stateFor renders the snapshot for one viewer ("" playerID = host/spectator).
func (g *gameData) stateFor(now int64, playerID string) stateMsg {
	status, q := g.status(), g.q()
	sg := stateGame{
		Pin: g.Pin, Status: status, QuizTitle: g.Meta["quizTitle"], QIndex: q, QCount: len(g.Questions),
		Answered: len(g.Answers), PlayerCount: len(g.Players),
		Players: make([]statePlayer, 0, len(g.Players)),
	}
	for id, p := range g.Players {
		sg.Players = append(sg.Players, statePlayer{ID: id, Nick: p.Nick})
	}
	sort.Slice(sg.Players, func(i, j int) bool {
		a, b := strings.ToLower(sg.Players[i].Nick), strings.ToLower(sg.Players[j].Nick)
		if a != b {
			return a < b
		}
		return sg.Players[i].ID < sg.Players[j].ID
	})

	rank := g.ranked()
	sg.Scoreboard = rank
	if status != "finished" && len(rank) > 10 {
		sg.Scoreboard = rank[:10]
	}

	var cur *Question
	if q >= 0 && q < len(g.Questions) && status != "lobby" {
		cur = &g.Questions[q]
		sg.Question = &stateQuestion{Text: cur.Text, Choices: cur.Choices, TimeLimit: cur.TimeLimit, Deadline: g.deadline(), ServerNow: now}
	} else {
		// clients still get serverNow for timer sync even without a question
		sg.Question = nil
	}
	revealed := status == "reveal" || status == "finished"
	if revealed && cur != nil {
		counts := make([]int, len(cur.Choices))
		for _, a := range g.Answers {
			if a.Choice >= 0 && a.Choice < len(counts) {
				counts[a.Choice]++
			}
		}
		sg.Reveal = &stateReveal{Correct: cur.Correct, Counts: counts}
	}

	if playerID != "" {
		if p, ok := g.Players[playerID]; ok {
			you := &stateYou{ID: playerID, Nick: p.Nick}
			for _, s := range rank {
				if s.ID == playerID {
					you.Score = s.Score
				}
			}
			if a, ok := g.Answers[playerID]; ok {
				you.Answered = true
				if revealed && cur != nil {
					pts, okc := a.Points, a.Choice == cur.Correct
					you.LastPoints, you.LastCorrect = &pts, &okc
				}
			}
			sg.You = you
		}
	}
	return stateMsg{Type: "state", V: g.v(), Game: sg}
}

// points implements the contract formula: round(1000*(1-elapsed/limit/2)).
func points(correct bool, elapsedMs, limitMs int64) int {
	if !correct || limitMs <= 0 {
		return 0
	}
	if elapsedMs < 0 {
		elapsedMs = 0
	}
	if elapsedMs > limitMs {
		elapsedMs = limitMs
	}
	return int(math.Round(1000 * (1 - float64(elapsedMs)/float64(limitMs)/2)))
}
