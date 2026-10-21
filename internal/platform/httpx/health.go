// Package httpx holds the HTTP plumbing shared by the api binary.
package httpx

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// Pinger reports whether a dependency (Postgres) is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

const readyTimeout = 2 * time.Second

// NewRouter builds the root router with liveness and readiness probes.
func NewRouter(db Pinger) http.Handler { return NewRouterWith(db, nil) }

// NewRouterWith is NewRouter plus a hook that mounts additional routes (the /v1 API).
func NewRouterWith(db Pinger, mount func(chi.Router)) http.Handler {
	r := chi.NewRouter()
	r.Use(securityHeaders)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), readyTimeout)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	if mount != nil {
		mount(r)
	}
	return r
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
