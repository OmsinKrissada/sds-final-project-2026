package main

// ws.go: WebSocket endpoint and per-socket writer/ping machinery.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/quizbox/quizbox/internal/common"
	"github.com/redis/go-redis/v9"
)

const (
	sendBuffer   = 32
	pingEvery    = 10 * time.Second
	pingTimeout  = 5 * time.Second
	pingMaxMiss  = 2
	writeTimeout = 5 * time.Second
)

type client struct {
	conn     *websocket.Conn
	pin      string
	role     string // host | player
	playerID string
	out      chan []byte
	cancel   context.CancelFunc
	once     sync.Once
}

// enqueue never blocks: a socket whose buffer is full is dropped (it reconnects
// and gets a fresh snapshot).
func (c *client) enqueue(b []byte) {
	select {
	case c.out <- b:
	default:
		go c.closeWith(4503, "reconnect")
	}
}

func (c *client) sendJSON(v any) {
	if b, err := json.Marshal(v); err == nil {
		c.enqueue(b)
	}
}

func (c *client) sendError(msg string) {
	c.sendJSON(map[string]string{"type": "error", "error": msg})
}

// closeWith closes the socket with a close frame (first caller wins).
func (c *client) closeWith(code websocket.StatusCode, reason string) {
	c.once.Do(func() {
		c.cancel()
		_ = c.conn.Close(code, reason)
	})
}

func (c *client) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-c.out:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				c.closeWith(websocket.StatusGoingAway, "write failed")
				return
			}
		}
	}
}

// pingLoop sends protocol ping frames; two missed pongs drop the socket.
func (c *client) pingLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	miss := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := c.conn.Ping(pctx)
			cancel()
			if err == nil {
				miss = 0
			} else if miss++; miss >= pingMaxMiss && ctx.Err() == nil {
				c.closeWith(websocket.StatusGoingAway, "ping timeout")
				return
			}
		}
	}
}

type helloMsg struct {
	Type        string `json:"type"`
	Trace       string `json:"trace"`
	Hops        int    `json:"hops"`
	PlayerID    string `json:"playerId,omitempty"`
	PlayerToken string `json:"playerToken,omitempty"`
}

func (h *Hub) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	conn.SetReadLimit(8 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	reject := func(code int, reason string) { _ = conn.Close(websocket.StatusCode(code), reason) }

	pin := r.PathValue("pin")
	qp := r.URL.Query()
	role := qp.Get("role")
	if role != "host" && role != "player" {
		reject(4400, "bad role")
		return
	}
	meta, err := h.ensureGame(ctx, pin)
	if errors.Is(err, errNotFound) {
		reject(4404, "no such game")
		return
	} else if err != nil {
		reject(4503, "reconnect")
		return
	}

	c := &client{conn: conn, pin: pin, role: role, out: make(chan []byte, sendBuffer), cancel: cancel}
	hello := helloMsg{Type: "hello"}

	switch role {
	case "host":
		tok := qp.Get("token")
		if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(meta["hostToken"])) != 1 {
			reject(4401, "bad host token")
			return
		}
	case "player":
		if resume := qp.Get("resume"); resume != "" {
			pid, err := h.rdb.HGet(ctx, kTokens(pin), resume).Result()
			common.Note(ctx, "redis")
			if errors.Is(err, redis.Nil) {
				reject(4401, "bad player token")
				return
			} else if err != nil {
				reject(4503, "reconnect")
				return
			}
			c.playerID, hello.PlayerID, hello.PlayerToken = pid, pid, resume
		} else {
			nick := strings.TrimSpace(qp.Get("nick"))
			if n := len([]rune(nick)); n < 1 || n > 20 {
				reject(4400, "nick must be 1-20 characters")
				return
			}
			var uid *int64
			if v, err := strconv.ParseInt(r.Header.Get(common.HeaderUserID), 10, 64); err == nil && v > 0 {
				uid = &v
			}
			pid, tok, err := h.join(ctx, pin, nick, uid)
			switch {
			case errors.Is(err, errNotFound):
				reject(4404, "no such game")
				return
			case errors.Is(err, errFull), errors.Is(err, errFinished):
				reject(4400, err.Error())
				return
			case err != nil:
				reject(4503, "reconnect")
				return
			}
			c.playerID, hello.PlayerID, hello.PlayerToken = pid, pid, tok
		}
	}

	trace := common.TraceSnapshot(r.Context())
	hello.Trace, hello.Hops = trace, common.AppHops(trace)
	c.sendJSON(hello)
	go c.writeLoop(ctx)
	go c.pingLoop(ctx, pingEvery)
	h.register(c)
	defer h.unregister(c)
	defer c.closeWith(websocket.StatusNormalClosure, "bye")

	if err := h.sendInitial(ctx, c); err != nil {
		c.sendError("state unavailable")
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var m struct {
			Type string `json:"type"`
			answerMsg
		}
		if json.Unmarshal(data, &m) != nil {
			c.sendError("bad message")
			continue
		}
		h.handleMessage(ctx, c, m.Type, m.answerMsg)
	}
}

func (h *Hub) handleMessage(ctx context.Context, c *client, typ string, am answerMsg) {
	switch typ {
	case "ping":
		c.sendJSON(map[string]string{"type": "pong"})
	case "start", "reveal", "next", "end":
		if c.role != "host" {
			c.sendError("host only")
			return
		}
		if err := h.hostAction(ctx, c.pin, typ); err != nil {
			c.sendError(err.Error())
		}
	case "answer":
		if c.role != "player" {
			c.sendError("players only")
			return
		}
		if err := h.answer(ctx, c.pin, c.playerID, am); err != nil {
			c.sendError(err.Error())
		}
	default:
		c.sendError("unknown message type")
	}
}
