package main

// hub.go: the per-replica hub. It owns the local sockets, applies host/player
// actions to Redis atomically, and fans state out after every game:events message.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/quizbox/quizbox/internal/common"
	"github.com/redis/go-redis/v9"
)

func logWarn(msg string, args ...any) { slog.Warn(msg, args...) }
func logInfo(msg string, args ...any) { slog.Info(msg, args...) }

// Hub is one game replica. Tests build several against one Redis.
type Hub struct {
	rdb     *redis.Client
	db      *sql.DB
	quiz    *common.Client
	account *common.Client
	node    string

	now      func() int64 // unix ms; replaced in tests
	debounce time.Duration

	mu     sync.Mutex
	conns  map[string]map[*client]struct{}
	timers map[string]*time.Timer
	ready  chan struct{} // closed after the first successful SUBSCRIBE
	rdyOne sync.Once
}

func NewHub(rdb *redis.Client, db *sql.DB, quiz, account *common.Client) *Hub {
	return &Hub{
		rdb: rdb, db: db, quiz: quiz, account: account, node: common.NodeName(),
		now:      func() int64 { return time.Now().UnixMilli() },
		debounce: 40 * time.Millisecond,
		conns:    map[string]map[*client]struct{}{},
		timers:   map[string]*time.Timer{},
		ready:    make(chan struct{}),
	}
}

// ---- socket registry and fan-out -------------------------------------------------

func (h *Hub) register(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[c.pin] == nil {
		h.conns[c.pin] = map[*client]struct{}{}
	}
	h.conns[c.pin][c] = struct{}{}
}

func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns[c.pin], c)
	if len(h.conns[c.pin]) == 0 {
		delete(h.conns, c.pin)
	}
}

func (h *Hub) localClients(pin string) []*client {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*client, 0, len(h.conns[pin]))
	for c := range h.conns[pin] {
		out = append(out, c)
	}
	return out
}

// CloseAll closes every local socket (SIGTERM: tell clients to reconnect elsewhere).
func (h *Hub) CloseAll(code websocket.StatusCode, reason string) {
	h.mu.Lock()
	var all []*client
	for _, m := range h.conns {
		for c := range m {
			all = append(all, c)
		}
	}
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range all {
		wg.Add(1)
		go func(c *client) { defer wg.Done(); c.closeWith(code, reason) }(c)
	}
	wg.Wait()
}

// schedule debounces: many events for a pin within the window cause one push.
func (h *Hub) schedule(pin string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.conns[pin]) == 0 || h.timers[pin] != nil {
		return
	}
	h.timers[pin] = time.AfterFunc(h.debounce, func() {
		h.mu.Lock()
		delete(h.timers, pin)
		h.mu.Unlock()
		h.pushPin(pin)
	})
}

// refreshAll pushes fresh state to every local socket (after a Redis reconnect,
// when events may have been missed).
func (h *Hub) refreshAll() {
	h.mu.Lock()
	pins := make([]string, 0, len(h.conns))
	for p := range h.conns {
		pins = append(pins, p)
	}
	h.mu.Unlock()
	for _, p := range pins {
		h.schedule(p)
	}
}

// pushPin loads the game once and sends each local socket its personalized state.
func (h *Hub) pushPin(pin string) {
	clients := h.localClients(pin)
	if len(clients) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g, err := h.loadGame(ctx, pin)
	if errors.Is(err, errNotFound) {
		for _, c := range clients {
			go c.closeWith(4404, "game gone")
		}
		return
	}
	if err != nil {
		logWarn("push: load failed", "pin", pin, "err", err)
		return
	}
	now := h.now()
	for _, c := range clients {
		c.sendJSON(g.stateFor(now, c.playerID))
	}
}

// sendInitial sends one socket its first state snapshot.
func (h *Hub) sendInitial(ctx context.Context, c *client) error {
	g, err := h.loadGame(ctx, c.pin)
	if err != nil {
		return err
	}
	c.sendJSON(g.stateFor(h.now(), c.playerID))
	return nil
}

// RunSubscriber listens on game:events until ctx ends. go-redis re-dials and
// re-subscribes on its own after a Redis restart; each (re)subscribe
// confirmation triggers a full refresh since events may have been missed.
func (h *Hub) RunSubscriber(ctx context.Context) {
	for ctx.Err() == nil {
		ps := h.rdb.Subscribe(ctx, eventsChannel)
		go func() { <-ctx.Done(); ps.Close() }()
		for item := range ps.ChannelWithSubscriptions() {
			switch m := item.(type) {
			case *redis.Subscription:
				if m.Kind == "subscribe" {
					h.rdyOne.Do(func() { close(h.ready) })
					h.refreshAll()
				}
			case *redis.Message:
				h.schedule(m.Payload)
			}
		}
		ps.Close()
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ---- host actions ------------------------------------------------------------------

type transition struct {
	allowed  string // comma separated statuses we may move from
	expectQ  string // "" = any
	status   string
	q        int
	qStart   int64
	deadline int64
}

// hostAction applies start/reveal/next/end. The Lua script is the only writer of
// status, so double clicks and racing replicas cannot advance twice.
func (h *Hub) hostAction(ctx context.Context, pin, action string) error {
	g, err := h.loadGame(ctx, pin)
	if err != nil {
		return err
	}
	st, q := g.status(), g.q()
	now := h.now()
	var t transition
	switch action {
	case "start":
		if len(g.Questions) == 0 {
			return errors.New("quiz has no questions")
		}
		t = h.questionTransition("lobby", "", 0, g.Questions[0], now)
	case "reveal":
		t = transition{allowed: "question", expectQ: strconv.Itoa(q), status: "reveal", q: q, qStart: g.qStart(), deadline: g.deadline()}
	case "next":
		if q+1 < len(g.Questions) {
			t = h.questionTransition("reveal", strconv.Itoa(q), q+1, g.Questions[q+1], now)
		} else {
			t = transition{allowed: "reveal", expectQ: strconv.Itoa(q), status: "finished", q: q, qStart: g.qStart(), deadline: g.deadline()}
		}
	case "end":
		t = transition{allowed: "lobby,question,reveal", status: "finished", q: q, qStart: g.qStart(), deadline: g.deadline()}
	default:
		return fmt.Errorf("unknown action %q", action)
	}

	res, err := transitionScript.Run(ctx, h.rdb, []string{kMeta(pin)},
		t.allowed, t.expectQ, t.status, t.q, t.qStart, t.deadline, ttlSecs()).Int64()
	common.Note(ctx, "redis")
	if err != nil {
		return err
	}
	switch res {
	case -1:
		return errNotFound
	case -2:
		return fmt.Errorf("cannot %s while game is %s", action, st)
	}
	h.touch(ctx, pin, len(g.Questions))
	h.publish(ctx, pin)
	if t.status == "finished" {
		h.finishGame(ctx, pin)
	} else {
		h.persist(ctx, pin)
	}
	return nil
}

func (h *Hub) questionTransition(from, expectQ string, q int, qu Question, now int64) transition {
	return transition{allowed: from, expectQ: expectQ, status: "question", q: q,
		qStart: now, deadline: now + int64(qu.TimeLimit)*1000}
}

// ---- answers -----------------------------------------------------------------------

type answerMsg struct {
	Q      *int `json:"q"`
	Choice *int `json:"choice"`
}

func (h *Hub) answer(ctx context.Context, pin, playerID string, m answerMsg) error {
	if m.Q == nil || m.Choice == nil {
		return errors.New("answer needs q and choice")
	}
	g, err := h.loadGame(ctx, pin)
	if err != nil {
		return err
	}
	q := g.q()
	if g.status() != "question" || *m.Q != q || q < 0 || q >= len(g.Questions) {
		return errors.New("question is not open")
	}
	qu := g.Questions[q]
	now := h.now()
	if now > g.deadline()+answerGraceMs {
		return errors.New("too late")
	}
	if *m.Choice < 0 || *m.Choice >= len(qu.Choices) {
		return errors.New("invalid choice")
	}
	if _, ok := g.Players[playerID]; !ok {
		return errors.New("unknown player")
	}
	elapsed := now - g.qStart()
	pts := points(*m.Choice == qu.Correct, elapsed, int64(qu.TimeLimit)*1000)
	raw, _ := json.Marshal(answerRec{Choice: *m.Choice, MS: elapsed, Points: pts})
	res, err := answerScript.Run(ctx, h.rdb, []string{kMeta(pin), kAns(pin, q), kScores(pin)},
		q, g.qStart(), playerID, string(raw), pts, ttlSecs()).Int64()
	common.Note(ctx, "redis")
	if err != nil {
		return err
	}
	switch res {
	case 0:
		return errors.New("already answered")
	case -1:
		return errors.New("question is not open")
	}
	h.publish(ctx, pin)
	return nil
}
