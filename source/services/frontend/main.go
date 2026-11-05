// Command frontend serves the Quizbox single-page app, injecting bootstrap
// data (session, hall of fame, game info, trace) into the embedded index.html.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"

	"github.com/quizbox/quizbox/internal/common"
)

//go:embed dist/index.html
var indexHTML []byte

var bootMarker = []byte("<!--BOOT-->")

var pinRe = regexp.MustCompile(`^[0-9]{4,10}$`)

type server struct {
	account *common.Client
	game    *common.Client
	index   []byte
}

// boot is the object exposed to the SPA as window.__BOOT__.
type boot struct {
	Session    any      `json:"session"`
	HallOfFame any      `json:"hallOfFame"`
	Game       any      `json:"game"`
	Degraded   []string `json:"degraded"`
	Recovering string   `json:"recovering,omitempty"`
	Trace      string   `json:"trace"`
}

func main() {
	common.HandleNoop()
	s := &server{
		account: common.NewClient(common.Env("ACCOUNT_URL", "http://localhost:8082")),
		game:    common.NewClient(common.Env("GAME_URL", "http://localhost:8084")),
		index:   indexHTML,
	}
	common.Run("frontend", s.routes())
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", common.Health(nil))
	mux.HandleFunc("GET /", s.page)
	return mux
}

// unavailable reports whether err means the downstream is down or erroring
// (as opposed to a clean 4xx such as 404 for an unknown game).
func unavailable(err error) bool {
	if err == nil {
		return false
	}
	var he *common.HTTPError
	if errors.As(err, &he) {
		return he.Status >= 500
	}
	return true
}

func (s *server) page(w http.ResponseWriter, r *http.Request) {
	if path.Ext(r.URL.Path) != "" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	hdr := http.Header{}
	if c := r.Header.Get("Cookie"); c != "" {
		hdr.Set("Cookie", c)
	}
	b := boot{Degraded: []string{}}
	status := http.StatusOK

	var sess any
	if err := s.account.GetJSON(ctx, "/internal/session", hdr, &sess); err != nil {
		slog.Warn("account session failed", "err", err)
		b.Recovering = "account"
		status = http.StatusServiceUnavailable
	} else {
		b.Session = sess
	}

	{
		switch r.URL.Path {
		case "/":
			var hof any
			if err := s.game.GetJSON(ctx, "/api/leaderboard?limit=5", nil, &hof); err != nil {
				if unavailable(err) {
					b.Degraded = append(b.Degraded, "game")
				}
			} else {
				b.HallOfFame = hof
			}
		case "/play":
			if pin := r.URL.Query().Get("pin"); pinRe.MatchString(pin) {
				var g any
				if err := s.game.GetJSON(ctx, "/api/games/"+url.PathEscape(pin), nil, &g); err != nil {
					if unavailable(err) {
						b.Degraded = append(b.Degraded, "game")
					}
				} else {
					b.Game = g
				}
			}
		}
	}
	b.Trace = common.TraceSnapshot(ctx)

	// json.Marshal escapes <, > and & as < etc. (and U+2028/2029), so the
	// payload can never terminate the surrounding <script> element.
	js, err := json.Marshal(b)
	if err != nil {
		js = []byte(`{"session":null,"degraded":[],"trace":""}`)
	}
	js = bytes.ReplaceAll(js, []byte("<"), []byte(`<`))
	inject := append(append([]byte("<script>window.__BOOT__="), js...), []byte("</script>")...)
	out := bytes.Replace(s.index, bootMarker, inject, 1)

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}
