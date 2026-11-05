// Package common holds the small shared toolkit every Quizbox service uses:
// request tracing (X-Trace / X-App-Hops), JSON helpers, health checks,
// config from env, and a server runner with graceful shutdown.
package common

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Header names used across the system.
const (
	HeaderTrace   = "X-Trace"    // comma-separated hop tokens, e.g. "gateway@pi-2,account@pi-3,redis"
	HeaderAppHops = "X-App-Hops" // distinct app containers excluding gateway and postgres (set by gateway)
	HeaderUserID  = "X-User-Id"
	HeaderUser    = "X-User-Name"
)

type traceKey struct{}

// Trace collects hop tokens for one inbound request.
type Trace struct {
	mu   sync.Mutex
	self string
	hops []string
}

func newTrace(self string) *Trace { return &Trace{self: self} }

// Add appends hop tokens (deduplicated, order of first appearance).
func (t *Trace) Add(tokens ...string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" || tok == t.self {
			continue
		}
		dup := false
		for _, h := range t.hops {
			if h == tok {
				dup = true
				break
			}
		}
		if !dup {
			t.hops = append(t.hops, tok)
		}
	}
}

// AddHeader merges a downstream X-Trace header value.
func (t *Trace) AddHeader(v string) {
	if v == "" {
		return
	}
	t.Add(strings.Split(v, ",")...)
}

// String renders self first, then collected hops.
func (t *Trace) String() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(append([]string{t.self}, t.hops...), ",")
}

// FromContext returns the request's Trace (nil-safe methods if absent).
func FromContext(ctx context.Context) *Trace {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	return t
}

// Note records a backing dependency touched while serving the request,
// e.g. common.Note(ctx, "redis") or common.Note(ctx, "postgres").
func Note(ctx context.Context, tokens ...string) { FromContext(ctx).Add(tokens...) }

// WithTrace attaches a fresh trace to ctx (useful for background work / tests).
func WithTrace(ctx context.Context, self string) (context.Context, *Trace) {
	t := newTrace(self)
	return context.WithValue(ctx, traceKey{}, t), t
}

// SelfToken is "<service>@<node>" using NODE_NAME (downward API) or hostname.
func SelfToken(service string) string { return service + "@" + NodeName() }

// AppHops counts distinct services in a trace string, excluding gateway and postgres.
func AppHops(trace string) int {
	seen := map[string]bool{}
	for _, tok := range strings.Split(trace, ",") {
		name := strings.TrimSpace(tok)
		if i := strings.IndexByte(name, '@'); i >= 0 {
			name = name[:i]
		}
		if name == "" || name == "gateway" || name == "postgres" {
			continue
		}
		seen[name] = true
	}
	return len(seen)
}

// TraceMiddleware creates a Trace per request and writes X-Trace on the
// response just before headers are sent. Downstream calls made with Client
// and Note() calls are folded in. Supports Hijack (WebSockets) and Flush.
func TraceMiddleware(service string, next http.Handler) http.Handler {
	self := SelfToken(service)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, t := WithTrace(r.Context(), self)
		tw := &traceWriter{ResponseWriter: w, t: t}
		next.ServeHTTP(tw, r.WithContext(ctx))
	})
}

type traceWriter struct {
	http.ResponseWriter
	t       *Trace
	written bool
}

func (w *traceWriter) inject() {
	if w.written {
		return
	}
	w.written = true
	w.Header().Set(HeaderTrace, w.t.String())
}

func (w *traceWriter) WriteHeader(code int) { w.inject(); w.ResponseWriter.WriteHeader(code) }
func (w *traceWriter) Write(b []byte) (int, error) {
	w.inject()
	return w.ResponseWriter.Write(b)
}
func (w *traceWriter) Flush() {
	w.inject()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *traceWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.inject()
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}
func (w *traceWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// TraceSnapshot returns the current trace string for this request (e.g. to
// embed in a WebSocket hello message, since browsers cannot read 101 headers).
func TraceSnapshot(ctx context.Context) string { return FromContext(ctx).String() }

// HopsString is a convenience for headers.
func HopsString(trace string) string { return strconv.Itoa(AppHops(trace)) }
