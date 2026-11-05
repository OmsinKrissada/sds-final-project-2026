package main

// http.go: REST endpoints.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/quizbox/quizbox/internal/common"
)

var pinRe = regexp.MustCompile(`^\d{6}$`)

func (h *Hub) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/games", h.handleCreate)
	mux.HandleFunc("GET /api/games/{pin}", h.handleGetGame)
	mux.HandleFunc("GET /api/leaderboard", h.handleLeaderboard)
	mux.HandleFunc("GET /api/me/history", h.handleHistory)
	mux.HandleFunc("GET /ws/games/{pin}", h.handleWS)
	mux.HandleFunc("GET /healthz", common.Health(map[string]common.Check{
		"redis": func(ctx context.Context) error { return h.rdb.Ping(ctx).Err() },
		"postgres": func(ctx context.Context) error {
			if h.db == nil {
				return errors.New("no database")
			}
			return h.db.PingContext(ctx)
		},
	}))
	return mux
}

type quizDoc struct {
	ID        int64      `json:"id"`
	OwnerID   int64      `json:"ownerId"`
	Title     string     `json:"title"`
	Questions []Question `json:"questions"`
}

func (h *Hub) handleCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid, err := strconv.ParseInt(r.Header.Get(common.HeaderUserID), 10, 64)
	if err != nil {
		common.WriteError(w, http.StatusUnauthorized, "login required")
		return
	}
	var body struct {
		QuizID int64 `json:"quizId"`
	}
	if err := common.ReadJSON(r, &body); err != nil || body.QuizID <= 0 {
		common.WriteError(w, http.StatusBadRequest, "quizId required")
		return
	}

	var quiz quizDoc
	err = h.quiz.GetJSON(ctx, "/internal/quizzes/"+strconv.FormatInt(body.QuizID, 10), nil, &quiz)
	var he *common.HTTPError
	switch {
	case errors.Is(err, common.ErrUnavailable):
		common.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "quiz service unavailable", "recovering": "quiz"})
		return
	case errors.As(err, &he) && he.Status == http.StatusNotFound:
		common.WriteError(w, http.StatusNotFound, "quiz not found")
		return
	case err != nil:
		common.WriteError(w, http.StatusBadGateway, "quiz lookup failed")
		return
	}
	if quiz.OwnerID != uid {
		common.WriteError(w, http.StatusForbidden, "not your quiz")
		return
	}
	if len(quiz.Questions) == 0 {
		common.WriteError(w, http.StatusBadRequest, "quiz has no questions")
		return
	}
	for i := range quiz.Questions {
		if quiz.Questions[i].TimeLimit <= 0 {
			quiz.Questions[i].TimeLimit = 20
		}
	}
	qjson, _ := jsonMarshal(quiz.Questions)
	hostToken := randToken(24)

	for attempt := 0; attempt < 20; attempt++ {
		pin := randPIN()
		sid, err := h.insertSession(ctx, pin, quiz.ID, uid)
		if err != nil {
			if errors.Is(err, errConflict) || isUniqueViolation(err) {
				continue
			}
			logWarn("create: insert session", "err", err)
			common.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database unavailable", "recovering": "postgres"})
			return
		}
		fields := []any{"quizId", quiz.ID, "quizTitle", quiz.Title, "hostId", uid, "hostToken", hostToken,
			"status", "lobby", "q", -1, "deadline", 0, "qStart", 0, "sessionId", sid, "v", 1}
		args := append([]any{string(qjson), ttlSecs()}, fields...)
		ok, err := createScript.Run(ctx, h.rdb, []string{kMeta(pin), kQuestions(pin)}, args...).Int()
		common.Note(ctx, "redis")
		if err != nil {
			h.deleteSession(ctx, sid)
			common.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "redis unavailable", "recovering": "redis"})
			return
		}
		if ok == 0 { // PIN taken in Redis: release the PG row and retry
			h.deleteSession(ctx, sid)
			continue
		}
		h.persist(ctx, pin)
		common.WriteJSON(w, http.StatusCreated, map[string]string{"pin": pin, "hostToken": hostToken})
		return
	}
	common.WriteError(w, http.StatusServiceUnavailable, "could not allocate a PIN")
}

func (h *Hub) handleGetGame(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pin := r.PathValue("pin")
	if !pinRe.MatchString(pin) {
		common.WriteError(w, http.StatusNotFound, "game not found")
		return
	}
	meta, err := h.ensureGame(ctx, pin)
	if errors.Is(err, errNotFound) {
		common.WriteError(w, http.StatusNotFound, "game not found")
		return
	} else if err != nil {
		common.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "game store unavailable", "recovering": "redis"})
		return
	}
	n, _ := h.rdb.HLen(ctx, kPlayers(pin)).Result()
	common.WriteJSON(w, http.StatusOK, map[string]any{
		"pin": pin, "status": meta["status"], "quizTitle": meta["quizTitle"], "playerCount": n,
	})
}

func (h *Hub) handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := 10
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, 100)
	}
	h.ensureLeaderboard(ctx)
	zs, err := h.rdb.ZRevRangeWithScores(ctx, "lb:alltime", 0, int64(limit-1)).Result()
	common.Note(ctx, "redis")
	if err != nil {
		common.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "leaderboard unavailable", "recovering": "redis"})
		return
	}
	ids := make([]string, 0, len(zs))
	for _, z := range zs {
		ids = append(ids, z.Member.(string))
	}
	names := h.userNames(ctx, ids)
	type entry struct {
		UserID      int64  `json:"userId"`
		DisplayName string `json:"displayName"`
		Score       int64  `json:"score"`
	}
	entries := make([]entry, 0, len(zs))
	for _, z := range zs {
		id, _ := strconv.ParseInt(z.Member.(string), 10, 64)
		name := names[id]
		if name == "" {
			name = fmt.Sprintf("player #%d", id)
		}
		entries = append(entries, entry{UserID: id, DisplayName: name, Score: int64(z.Score)})
	}
	common.WriteJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// userNames resolves display names via account; nil/empty map when account is down.
func (h *Hub) userNames(ctx context.Context, ids []string) map[int64]string {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out
	}
	var resp struct {
		Users []struct {
			ID          int64  `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"users"`
	}
	if err := h.account.GetJSON(ctx, "/internal/users?ids="+url.QueryEscape(strings.Join(ids, ",")), nil, &resp); err != nil {
		logWarn("account lookup failed", "err", err)
		return out
	}
	for _, u := range resp.Users {
		out[u.ID] = u.DisplayName
	}
	return out
}

func (h *Hub) handleHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid, err := strconv.ParseInt(r.Header.Get(common.HeaderUserID), 10, 64)
	if err != nil {
		common.WriteError(w, http.StatusUnauthorized, "login required")
		return
	}
	type played struct {
		Pin        string `json:"pin"`
		QuizTitle  string `json:"quizTitle"`
		Score      int    `json:"score"`
		Rank       int    `json:"rank"`
		FinishedAt string `json:"finishedAt"`
	}
	type hosted struct {
		Pin       string `json:"pin"`
		QuizTitle string `json:"quizTitle"`
		Status    string `json:"status"`
		CreatedAt string `json:"createdAt"`
	}
	resp := struct {
		Played []played `json:"played"`
		Hosted []hosted `json:"hosted"`
	}{Played: []played{}, Hosted: []hosted{}}

	rows, err := h.db.QueryContext(ctx, `SELECT pin, quiz_title, score, rank, finished_at FROM game.results
WHERE player_id=$1 ORDER BY finished_at DESC, id DESC LIMIT 50`, uid)
	common.Note(ctx, "postgres")
	if err != nil {
		common.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "history unavailable", "recovering": "postgres"})
		return
	}
	for rows.Next() {
		var p played
		var t time.Time
		if rows.Scan(&p.Pin, &p.QuizTitle, &p.Score, &p.Rank, &t) == nil {
			p.FinishedAt = t.UTC().Format(time.RFC3339)
			resp.Played = append(resp.Played, p)
		}
	}
	rows.Close()

	rows, err = h.db.QueryContext(ctx, `SELECT pin, COALESCE(snapshot->'meta'->>'quizTitle',''), status, created_at
FROM game.sessions WHERE host_id=$1 ORDER BY created_at DESC, id DESC LIMIT 50`, uid)
	if err != nil {
		common.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "history unavailable", "recovering": "postgres"})
		return
	}
	for rows.Next() {
		var hd hosted
		var t time.Time
		if rows.Scan(&hd.Pin, &hd.QuizTitle, &hd.Status, &t) == nil {
			hd.CreatedAt = t.UTC().Format(time.RFC3339)
			resp.Hosted = append(resp.Hosted, hd)
		}
	}
	rows.Close()
	common.WriteJSON(w, http.StatusOK, resp)
}

var _ = sql.ErrNoRows
