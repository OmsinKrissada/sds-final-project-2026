// Command gateway is Quizbox's only public entry point: routing, forward
// auth against account, and trace aggregation (X-Trace / X-App-Hops).
package main

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/quizbox/quizbox/internal/common"
)

func main() {
	common.HandleNoop()
	h, err := newGateway(
		common.Env("ACCOUNT_URL", "http://localhost:8082"),
		common.Env("QUIZ_URL", "http://localhost:8083"),
		common.Env("GAME_URL", "http://localhost:8084"),
		common.Env("FRONTEND_URL", "http://localhost:8081"),
	)
	if err != nil {
		slog.Error("bad upstream url", "err", err)
		return
	}
	common.Run("gateway", h)
}

type authMode int

const (
	authNone authMode = iota
	authRequired
	authOptional
)

type upstream struct {
	name  string
	proxy *httputil.ReverseProxy
}

type gateway struct {
	account *common.Client
}

// reqInfo is per-request bookkeeping for the access log (stored in the
// context so the response writer never needs wrapping, keeping Hijack intact).
type reqInfo struct {
	upstream string
	status   int
}

type infoKey struct{}

func infoFrom(r *http.Request) *reqInfo {
	if i, _ := r.Context().Value(infoKey{}).(*reqInfo); i != nil {
		return i
	}
	return &reqInfo{}
}

func newGateway(accountURL, quizURL, gameURL, frontendURL string) (http.Handler, error) {
	g := &gateway{account: common.NewClient(accountURL)}
	mk := func(name, raw string) (*upstream, error) {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, err
		}
		return &upstream{name: name, proxy: newProxy(name, u)}, nil
	}
	account, err := mk("account", accountURL)
	if err != nil {
		return nil, err
	}
	quiz, err := mk("quiz", quizURL)
	if err != nil {
		return nil, err
	}
	game, err := mk("game", gameURL)
	if err != nil {
		return nil, err
	}
	frontend, err := mk("frontend", frontendURL)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", common.Health(nil))
	notFound := func(w http.ResponseWriter, r *http.Request) { common.WriteError(w, http.StatusNotFound, "not found") }
	mux.HandleFunc("/internal", notFound)
	mux.HandleFunc("/internal/", notFound)
	mux.Handle("/api/auth/", g.route(account, authNone))
	mux.Handle("/api/quizzes", g.route(quiz, authRequired))
	mux.Handle("/api/quizzes/", g.route(quiz, authRequired))
	mux.Handle("POST /api/games", g.route(game, authRequired))
	mux.Handle("GET /api/games/{pin}", g.route(game, authNone))
	mux.Handle("GET /api/leaderboard", g.route(game, authNone))
	mux.Handle("GET /api/me/history", g.route(game, authRequired))
	mux.Handle("/ws/", g.route(game, authOptional))
	mux.Handle("/", g.route(frontend, authNone))
	return mux, nil
}

// route authenticates (per mode) and proxies to up, logging one line.
func (g *gateway) route(up *upstream, mode authMode) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		inf := &reqInfo{upstream: up.name}
		r = r.WithContext(context.WithValue(r.Context(), infoKey{}, inf))
		defer func() {
			slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", inf.status,
				"upstream", up.name, "dur", time.Since(start).Round(time.Microsecond),
				"hops", common.HopsString(common.TraceSnapshot(r.Context())))
		}()

		// Never trust client-supplied identity.
		r.Header.Del(common.HeaderUserID)
		r.Header.Del(common.HeaderUser)
		if mode != authNone && !g.authenticate(w, r, mode) {
			return
		}
		up.proxy.ServeHTTP(w, r)
	})
}

// authenticate calls account /internal/authz. It returns false if it already
// wrote the response.
func (g *gateway) authenticate(w http.ResponseWriter, r *http.Request, mode authMode) bool {
	resp, err := g.account.Do(r.Context(), http.MethodGet, "/internal/authz",
		http.Header{"Cookie": r.Header.Values("Cookie")}, nil, nil)
	if err == nil {
		r.Header.Set(common.HeaderUserID, resp.Header.Get(common.HeaderUserID))
		r.Header.Set(common.HeaderUser, resp.Header.Get(common.HeaderUser))
		return true
	}
	var he *common.HTTPError
	if errors.As(err, &he) && he.Status == http.StatusUnauthorized {
		if mode == authOptional {
			return true
		}
		fail(w, r, http.StatusUnauthorized, map[string]string{"error": "login required"})
		return false
	}
	if mode == authOptional { // account trouble must not break anonymous play
		return true
	}
	fail(w, r, http.StatusServiceUnavailable, map[string]string{"error": "service unavailable", "recovering": "account"})
	return false
}

// fail writes a JSON error carrying X-App-Hops (X-Trace is added by TraceMiddleware).
func fail(w http.ResponseWriter, r *http.Request, code int, body map[string]string) {
	infoFrom(r).status = code
	w.Header().Set(common.HeaderAppHops, common.HopsString(common.TraceSnapshot(r.Context())))
	common.WriteJSON(w, code, body)
}

func newProxy(name string, target *url.URL) *httputil.ReverseProxy {
	base := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 15 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     30 * time.Second,
	}
	return &httputil.ReverseProxy{
		Transport: retryTransport{base},
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host // keep the public Host (frontend links, WS Origin checks)
			pr.SetXForwarded()
		},
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ModifyResponse: func(res *http.Response) error {
			inf := infoFrom(res.Request)
			inf.status = res.StatusCode
			tr := common.FromContext(res.Request.Context())
			tr.AddHeader(res.Header.Get(common.HeaderTrace))
			res.Header.Set(common.HeaderAppHops, common.HopsString(tr.String()))
			if res.StatusCode == http.StatusSwitchingProtocols {
				// ReverseProxy hijacks (TraceMiddleware injects the merged X-Trace
				// at that moment) and then *adds* res.Header; drop ours to avoid a duplicate.
				res.Header.Del(common.HeaderTrace)
			} else {
				res.Header.Set(common.HeaderTrace, tr.String())
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) { // client went away
				infoFrom(r).status = 499
				return
			}
			slog.Warn("upstream error", "upstream", name, "err", err)
			fail(w, r, http.StatusServiceUnavailable, map[string]string{"error": "service unavailable", "recovering": name})
		},
	}
}

// retryTransport retries a body-less GET/HEAD once, 150ms after a dial error.
type retryTransport struct{ base http.RoundTripper }

func (t retryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	var op *net.OpError
	if err == nil || !errors.As(err, &op) || op.Op != "dial" {
		return resp, err
	}
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || (r.Body != nil && r.Body != http.NoBody) {
		return resp, err
	}
	select {
	case <-time.After(150 * time.Millisecond):
	case <-r.Context().Done():
		return nil, err
	}
	return t.base.RoundTrip(r)
}
