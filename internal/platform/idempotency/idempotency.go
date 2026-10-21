// Package idempotency makes every state-changing HTTP request safe to retry (invariant 7).
//
// A request carrying an Idempotency-Key is claimed in platform.idempotency_keys. A retry with the same key and the
// same request returns the stored response; the same key with a different request is rejected with 422; a retry that
// arrives while the first is still running gets 409. The key is scoped to the authenticated principal.
//
// The middleware stops duplicate HTTP deliveries. Exactly-once money movement is additionally guaranteed by unique
// business keys in the database (tickets, payments and journals), so a crash between commit and response storage can
// never double-apply an effect (see ADR 0003).
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/itsivali/lottery-platform/internal/platform/httpx"
	"github.com/itsivali/lottery-platform/internal/platform/idempotency/store"
)

const (
	// HeaderKey is the request header carrying the idempotency key.
	HeaderKey = "Idempotency-Key"
	// HeaderReplay is set to "true" on a replayed response.
	HeaderReplay = "Idempotent-Replay"

	maxBody      = 1 << 20
	defaultTTL   = 24 * time.Hour
	defaultStale = 60 * time.Second
)

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9._:~=+/-]{1,255}$`)

// Outcome of trying to claim a key.
type Outcome int

// Claim outcomes.
const (
	Claimed  Outcome = iota // this caller owns the key and must run the request
	Replay                  // a finished response is stored
	Mismatch                // same key, different request
	InFlight                // the first request is still running
)

// Result is what Claim returns. The response fields are set for Replay.
type Result struct {
	Outcome     Outcome
	Status      int
	Body        []byte
	ContentType string
	Location    string
}

// Store persists claims in Postgres.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Store on pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Claim tries to take (principal, key) for a request with the given hash.
func (s *Store) Claim(ctx context.Context, principal, key string, hash []byte, ttl, staleAfter time.Duration) (Result, error) {
	q := store.New(s.pool)
	_, err := q.ClaimKey(ctx, store.ClaimKeyParams{
		Principal: principal, Key: key, RequestHash: hash, TtlSeconds: ttl.Seconds(), StaleSeconds: staleAfter.Seconds(),
	})
	if err == nil {
		return Result{Outcome: Claimed}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, fmt.Errorf("idempotency: claim: %w", err)
	}
	row, err := q.GetKey(ctx, store.GetKeyParams{Principal: principal, Key: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{Outcome: InFlight}, nil // expired between the two statements: treat as busy, the client retries
	}
	if err != nil {
		return Result{}, fmt.Errorf("idempotency: get: %w", err)
	}
	if !bytes.Equal(row.RequestHash, hash) {
		return Result{Outcome: Mismatch}, nil
	}
	if !row.CompletedAt.Valid || !row.ResponseStatus.Valid {
		return Result{Outcome: InFlight}, nil
	}
	return Result{
		Outcome: Replay, Status: int(row.ResponseStatus.Int16), Body: row.ResponseBody,
		ContentType: deref(row.ResponseContentType), Location: deref(row.ResponseLocation),
	}, nil
}

// Complete stores the finished response.
func (s *Store) Complete(ctx context.Context, principal, key string, hash []byte, status int, body []byte, contentType, location string) error {
	return store.New(s.pool).CompleteKey(ctx, store.CompleteKeyParams{
		Principal: principal, Key: key, RequestHash: hash, Status: pgtype.Int2{Int16: int16(status), Valid: true}, //nolint:gosec // HTTP status fits int16
		Body: body, ContentType: nilIfEmpty(contentType), Location: nilIfEmpty(location),
	})
}

// Release frees an unfinished claim so the client can retry immediately.
func (s *Store) Release(ctx context.Context, principal, key string, hash []byte) error {
	return store.New(s.pool).ReleaseKey(ctx, store.ReleaseKeyParams{Principal: principal, Key: key, RequestHash: hash})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Options tunes the middleware; zero values give production defaults.
type Options struct {
	TTL        time.Duration // how long a key (and its response) is remembered
	StaleAfter time.Duration // how long an unfinished claim blocks retries
}

// Middleware enforces idempotency on POST, PUT, PATCH and DELETE. principal identifies the caller (for example
// "user:<uuid>"); an empty principal is refused with 401 so a key can never be shared across callers.
func Middleware(s *Store, principal func(*http.Request) string, opts Options) func(http.Handler) http.Handler {
	if opts.TTL <= 0 {
		opts.TTL = defaultTTL
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = defaultStale
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !needsKey(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			key := r.Header.Get(HeaderKey)
			if !keyPattern.MatchString(key) {
				httpx.WriteProblem(w, http.StatusBadRequest, "idempotency-key-required", "Idempotency-Key required",
					"state-changing requests need an Idempotency-Key header of 1-255 URL-safe characters")
				return
			}
			who := principal(r)
			if who == "" {
				httpx.WriteProblem(w, http.StatusUnauthorized, "unauthenticated", "Authentication required", "")
				return
			}
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
			if err != nil {
				httpx.WriteProblem(w, http.StatusRequestEntityTooLarge, "body-too-large", "Request body too large", "")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			hash := requestHash(r, body)

			res, err := s.Claim(r.Context(), who, key, hash, opts.TTL, opts.StaleAfter)
			if err != nil {
				slog.ErrorContext(r.Context(), "idempotency claim failed", "err", err)
				httpx.WriteProblem(w, http.StatusServiceUnavailable, "unavailable", "Temporarily unavailable", "")
				return
			}
			switch res.Outcome {
			case Mismatch:
				httpx.WriteProblem(w, http.StatusUnprocessableEntity, "idempotency-key-reuse", "Idempotency key reused",
					"this key was already used with a different request")
			case InFlight:
				httpx.WriteProblemRetry(w, http.StatusConflict, "request-in-progress", "Request in progress",
					"a request with this key is still being processed", 1)
			case Replay:
				replay(w, res)
			case Claimed:
				serve(s, next, w, r, who, key, hash)
			}
		})
	}
}

func needsKey(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func requestHash(r *http.Request, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(r.Method))
	h.Write([]byte{0})
	h.Write([]byte(r.URL.Path))
	h.Write([]byte{0})
	h.Write([]byte(r.URL.RawQuery))
	h.Write([]byte{0})
	h.Write(body)
	return h.Sum(nil)
}

func replay(w http.ResponseWriter, res Result) {
	if res.ContentType != "" {
		w.Header().Set("Content-Type", res.ContentType)
	}
	if res.Location != "" {
		w.Header().Set("Location", res.Location)
	}
	w.Header().Set(HeaderReplay, "true")
	w.WriteHeader(res.Status)
	_, _ = w.Write(res.Body)
}

func serve(s *Store, next http.Handler, w http.ResponseWriter, r *http.Request, who, key string, hash []byte) {
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	done := false
	defer func() {
		// runs on panic too: free the claim so the client can retry
		if !done {
			_ = s.Release(context.WithoutCancel(r.Context()), who, key, hash)
		}
	}()
	next.ServeHTTP(rec, r)
	ctx := context.WithoutCancel(r.Context())
	if rec.status >= http.StatusInternalServerError || rec.overflow {
		_ = s.Release(ctx, who, key, hash)
		done = true
		return
	}
	if err := s.Complete(ctx, who, key, hash, rec.status, rec.buf.Bytes(), rec.contentType, rec.location); err != nil {
		slog.ErrorContext(ctx, "idempotency complete failed", "err", err)
	}
	done = true
}

// recorder passes the response through while keeping a copy of what must be replayed.
type recorder struct {
	http.ResponseWriter
	status      int
	wrote       bool
	buf         bytes.Buffer
	overflow    bool
	contentType string
	location    string
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.wrote = true
		r.status = code
		r.contentType = r.Header().Get("Content-Type")
		r.location = r.Header().Get("Location")
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	if r.buf.Len()+len(p) > maxBody {
		r.overflow = true
	} else {
		r.buf.Write(p)
	}
	return r.ResponseWriter.Write(p)
}
