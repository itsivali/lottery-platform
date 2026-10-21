// Package api assembles the public HTTP API: handlers over the identity, kyc and rg services, with authentication,
// idempotency and rate limiting applied uniformly. Modules stay free of HTTP; this package is the only place that
// knows routes and JSON shapes (api/openapi.yaml is the contract, and a test keeps the two in step).
package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/kyc"
	"github.com/itsivali/lottery-platform/internal/platform/httpx"
	"github.com/itsivali/lottery-platform/internal/platform/idempotency"
	"github.com/itsivali/lottery-platform/internal/platform/ratelimit"
	"github.com/itsivali/lottery-platform/internal/rg"
)

// Deps are the collaborators the API needs.
type Deps struct {
	Identity *identity.Service
	KYC      *kyc.Service
	RG       *rg.Service
	Idem     *idempotency.Store
	Limiter  *ratelimit.Limiter
	// TrustedProxyHeader is the header our load balancer sets with the client address ("" = use the socket address).
	TrustedProxyHeader string
}

type ctxKey struct{}

func principalFrom(ctx context.Context) (identity.Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(identity.Principal)
	return p, ok
}

// Mount registers the /v1 routes on r.
func Mount(d Deps) func(chi.Router) {
	h := &handlers{Deps: d}
	return func(r chi.Router) {
		r.Route("/v1", func(r chi.Router) {
			r.Use(h.optionalAuth)
			r.Use(idempotency.Middleware(d.Idem, h.idempotencyPrincipal, idempotency.Options{}))

			// public (no token required)
			r.Post("/auth/otp/request", h.otpRequest)
			r.Post("/auth/otp/verify", h.otpVerify)
			r.Post("/auth/refresh", h.refresh)
			r.Post("/auth/sso/{provider}", h.ssoSignIn)

			// authenticated
			r.Group(func(r chi.Router) {
				r.Use(requireAuth)
				r.Post("/auth/logout", h.logout)
				r.Post("/auth/sso/{provider}/link", h.ssoLink)
				r.Get("/kyc", h.kycStatus)
				r.Post("/kyc", h.kycSubmit)
				r.Get("/rg/limits", h.rgLimits)
				r.Put("/rg/limits", h.rgSetLimit)
				r.Post("/rg/timeout", h.rgTimeout)
				r.Post("/rg/self-exclusion", h.rgSelfExclude)
			})
		})
	}
}

type handlers struct{ Deps }

func (h *handlers) clientIP(r *http.Request) string { return httpx.ClientIP(r, h.TrustedProxyHeader) }

// optionalAuth attaches the principal when a valid bearer token is present; protected routes enforce it separately.
func (h *handlers) optionalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			if p, err := h.Identity.Authenticate(r.Context(), tok); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, p))
			}
		}
		next.ServeHTTP(w, r)
	})
}

func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := principalFrom(r.Context()); !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			httpx.WriteProblem(w, http.StatusUnauthorized, "unauthenticated", "Authentication required", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// idempotencyPrincipal scopes idempotency keys to the authenticated user, or to the client address before sign-in.
func (h *handlers) idempotencyPrincipal(r *http.Request) string {
	if p, ok := principalFrom(r.Context()); ok {
		return "user:" + p.UserID
	}
	return "anon:" + h.clientIP(r)
}

// limit applies a fail-closed sliding-window limit to a key and writes the problem response when exceeded.
func (h *handlers) limit(w http.ResponseWriter, r *http.Request, key string, n int, window time.Duration) bool {
	d, err := h.Limiter.Allow(r.Context(), key, n, window)
	if err != nil {
		httpx.WriteProblemRetry(w, http.StatusServiceUnavailable, "unavailable", "Temporarily unavailable", "", 5)
		return false
	}
	if !d.Allowed {
		secs := int(d.RetryAfter.Seconds()) + 1
		httpx.WriteProblemRetry(w, http.StatusTooManyRequests, "rate-limited", "Too many requests", "", secs)
		return false
	}
	return true
}
