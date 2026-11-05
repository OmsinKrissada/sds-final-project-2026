package main

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/quizbox/quizbox/internal/common"
)

const cookieName = "qb_session"

type server struct {
	st           *store
	cookieSecure bool
}

func (s *server) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/register", s.register)
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/logout", s.logout)
	mux.HandleFunc("GET /api/auth/me", s.me)
	mux.HandleFunc("GET /internal/authz", s.authz)
	mux.HandleFunc("GET /internal/session", s.session)
	mux.HandleFunc("GET /internal/users", s.users)
}

func (s *server) setCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func sessionToken(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func validateRegister(email, password, name string) string {
	if at := strings.IndexByte(email, '@'); at <= 0 || at == len(email)-1 || len(email) > 254 {
		return "email must be a valid address"
	}
	if utf8.RuneCountInString(password) < 6 {
		return "password must be at least 6 characters"
	}
	n := utf8.RuneCountInString(name)
	if n < 1 || n > 40 {
		return "displayName must be 1-40 characters"
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "displayName must not contain control characters"
		}
	}
	return ""
}

func (s *server) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"displayName"`
	}
	if err := common.ReadJSON(r, &in); err != nil {
		common.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	name := strings.TrimSpace(in.DisplayName)
	if msg := validateRegister(email, in.Password, name); msg != "" {
		common.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		slog.Error("hash password", "err", err)
		common.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	ctx := r.Context()
	tx, err := s.st.db.BeginTx(ctx, nil)
	if err != nil {
		common.WriteError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	defer tx.Rollback()

	id, err := s.st.insertUser(ctx, tx, email, name, hash)
	if isUniqueViolation(err) {
		common.WriteError(w, http.StatusConflict, "email already registered")
		return
	}
	if err != nil {
		slog.Error("insert user", "err", err)
		common.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	token, tokHash, exp, err := s.st.issueSession(ctx, tx, id)
	if err != nil {
		slog.Error("issue session", "err", err)
		common.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(); err != nil {
		common.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	u := User{ID: id, Email: email, DisplayName: name}
	s.st.cacheSession(ctx, tokHash, u, exp)

	s.setCookie(w, token)
	common.WriteJSON(w, http.StatusCreated, map[string]any{"user": u})
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := common.ReadJSON(r, &in); err != nil {
		common.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" || in.Password == "" {
		common.WriteError(w, http.StatusBadRequest, "email and password are required")
		return
	}
	ctx := r.Context()
	u, hash, err := s.st.userByEmail(ctx, email)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !verifyPassword(in.Password, hash)) {
		common.WriteError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if err != nil {
		slog.Error("lookup user", "err", err)
		common.WriteError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	token, tokHash, exp, err := s.st.issueSession(ctx, s.st.db, u.ID)
	if err != nil {
		slog.Error("issue session", "err", err)
		common.WriteError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	s.st.cacheSession(ctx, tokHash, u, exp)

	s.setCookie(w, token)
	common.WriteJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if token := sessionToken(r); token != "" {
		if err := s.st.deleteSession(r.Context(), token); err != nil {
			slog.Error("delete session", "err", err)
			common.WriteError(w, http.StatusServiceUnavailable, "service unavailable")
			return
		}
	}
	s.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, ok, err := s.st.resolveSession(ctx, sessionToken(r))
	if err != nil {
		common.WriteError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	if !ok {
		common.WriteError(w, http.StatusUnauthorized, "login required")
		return
	}
	if u.Email == "" {
		if u, err = s.st.userByID(ctx, u.ID); err != nil {
			common.WriteError(w, http.StatusServiceUnavailable, "service unavailable")
			return
		}
	}
	common.WriteJSON(w, http.StatusOK, map[string]any{"user": u})
}

// authz is called by the gateway. It never reveals the email; the gateway
// only forwards id and name.
func (s *server) authz(w http.ResponseWriter, r *http.Request) {
	u, ok, err := s.st.resolveSession(r.Context(), sessionToken(r))
	if err != nil {
		common.WriteError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	if !ok {
		common.WriteError(w, http.StatusUnauthorized, "login required")
		return
	}
	w.Header().Set(common.HeaderUserID, strconv.FormatInt(u.ID, 10))
	w.Header().Set(common.HeaderUser, u.DisplayName)
	common.WriteJSON(w, http.StatusOK, map[string]any{"user": u})
}

// session is used by the frontend for SSR. Always 200; user is null when
// logged out or when the lookup fails.
func (s *server) session(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var user any
	u, ok, err := s.st.resolveSession(ctx, sessionToken(r))
	if err != nil {
		// Both Redis and Postgres failed: tell the frontend so it can show the
		// "account recovering" banner instead of silently rendering logged-out.
		slog.Warn("session lookup failed", "err", err)
		common.WriteError(w, http.StatusServiceUnavailable, "session store unavailable")
		return
	} else if ok {
		if u.Email == "" {
			if full, uerr := s.st.userByID(ctx, u.ID); uerr == nil {
				u = full
			} else {
				slog.Warn("user lookup failed", "err", uerr)
				u, ok = User{}, false
			}
		}
		if ok {
			user = u
		}
	}
	common.WriteJSON(w, http.StatusOK, map[string]any{"user": user})
}

const maxUserIDs = 100

func (s *server) users(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("ids"))
	ids := []int64{}
	if raw != "" {
		parts := strings.Split(raw, ",")
		if len(parts) > maxUserIDs {
			common.WriteError(w, http.StatusBadRequest, "too many ids")
			return
		}
		for _, p := range parts {
			id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
			if err != nil {
				common.WriteError(w, http.StatusBadRequest, "ids must be comma-separated integers")
				return
			}
			ids = append(ids, id)
		}
	}
	users := []User{}
	if len(ids) > 0 {
		var err error
		if users, err = s.st.usersByIDs(r.Context(), ids); err != nil {
			slog.Error("users lookup", "err", err)
			common.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	common.WriteJSON(w, http.StatusOK, map[string]any{"users": users})
}
