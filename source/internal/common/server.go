package common

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// Env returns the environment variable or def.
func Env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// EnvInt returns an int env var or def.
func EnvInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// NodeName is the Kubernetes node (downward API NODE_NAME) or hostname.
func NodeName() string {
	if n := os.Getenv("NODE_NAME"); n != "" {
		return n
	}
	h, _ := os.Hostname()
	return h
}

// PodName is POD_NAME or hostname.
func PodName() string {
	if n := os.Getenv("POD_NAME"); n != "" {
		return n
	}
	h, _ := os.Hostname()
	return h
}

// HandleNoop makes the binary exit 0 when run as "<bin> noop". The image
// pre-pull DaemonSet uses this to pull every image on every Pi.
func HandleNoop() {
	if len(os.Args) > 1 && os.Args[1] == "noop" {
		os.Exit(0)
	}
}

// WriteJSON writes v as JSON with status code.
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// WriteError writes {"error": msg}.
func WriteError(w http.ResponseWriter, code int, msg string) {
	WriteJSON(w, code, map[string]string{"error": msg})
}

// ReadJSON decodes a request body (max 1 MiB) into v.
func ReadJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

// Check is a readiness dependency check.
type Check func(ctx context.Context) error

// Health returns a /healthz handler that runs checks with a 1s timeout.
func Health(checks map[string]Check) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		res := map[string]string{}
		ok := true
		for name, c := range checks {
			if err := c(ctx); err != nil {
				res[name] = err.Error()
				ok = false
			} else {
				res[name] = "ok"
			}
		}
		res["pod"] = PodName()
		res["node"] = NodeName()
		code := http.StatusOK
		if !ok {
			code = http.StatusServiceUnavailable
		}
		WriteJSON(w, code, res)
	}
}

// Run serves h on PORT (default 8080), wrapped in TraceMiddleware, and shuts
// down gracefully on SIGTERM. No Read/WriteTimeout so WebSockets live on.
func Run(service string, h http.Handler) {
	addr := ":" + Env("PORT", "8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           TraceMiddleware(service, h),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		slog.Info("listening", "service", service, "addr", addr, "node", NodeName(), "pod", PodName())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	slog.Info("shutting down", "service", service)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, f := range shutdownHooks {
		f()
	}
	_ = srv.Shutdown(ctx)
}

var shutdownHooks []func()

// OnShutdown registers f to run on SIGTERM before HTTP shutdown (e.g. send
// WebSocket close frames telling clients to reconnect elsewhere).
func OnShutdown(f func()) { shutdownHooks = append(shutdownHooks, f) }
