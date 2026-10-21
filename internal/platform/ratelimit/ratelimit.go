// Package ratelimit is a Valkey-backed sliding-window rate limiter.
//
// Valkey holds only counters, so losing it never loses money or results. Routes that move money or play the game
// fail closed (503) when the limiter is unreachable; read routes may fail open.
package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/itsivali/lottery-platform/internal/platform/httpx"
)

// The script uses Valkey's own clock so that all API instances agree on the window, and runs atomically.
var script = valkey.NewLuaScript(`
local key, limit, window_ms, member = KEYS[1], tonumber(ARGV[1]), tonumber(ARGV[2]), ARGV[3]
local t = redis.call('TIME')
local now = t[1] * 1000 + math.floor(t[2] / 1000)
redis.call('ZREMRANGEBYSCORE', key, 0, now - window_ms)
local count = redis.call('ZCARD', key)
if count < limit then
  redis.call('ZADD', key, now, member)
  redis.call('PEXPIRE', key, window_ms)
  return {1, limit - count - 1, 0}
end
local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
return {0, 0, tonumber(oldest[2]) + window_ms - now}
`)

// Decision is the outcome of one Allow call.
type Decision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// Limiter checks and records requests.
type Limiter struct{ client valkey.Client }

// New returns a Limiter on client.
func New(client valkey.Client) *Limiter { return &Limiter{client: client} }

// Allow records a request against key and reports whether it is within limit requests per window.
func (l *Limiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (Decision, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Decision{}, err
	}
	res := script.Exec(ctx, l.client, []string{"rl:" + key},
		[]string{strconv.Itoa(limit), strconv.FormatInt(window.Milliseconds(), 10), hex.EncodeToString(b[:])})
	arr, err := res.ToArray()
	if err != nil {
		return Decision{}, fmt.Errorf("ratelimit: %w", err)
	}
	if len(arr) != 3 {
		return Decision{}, errors.New("ratelimit: unexpected script reply")
	}
	allowed, err := arr[0].AsInt64()
	if err != nil {
		return Decision{}, err
	}
	remaining, err := arr[1].AsInt64()
	if err != nil {
		return Decision{}, err
	}
	retryMS, err := arr[2].AsInt64()
	if err != nil {
		return Decision{}, err
	}
	return Decision{Allowed: allowed == 1, Remaining: int(remaining), RetryAfter: time.Duration(retryMS) * time.Millisecond}, nil
}

// Rule describes one limit applied by Middleware.
type Rule struct {
	Name       string        // part of the Valkey key, e.g. "otp", "play", "topup"
	Limit      int           // requests allowed per Window
	Window     time.Duration // sliding window
	FailClosed bool          // true for money and play routes: refuse with 503 when Valkey is unavailable
}

// Middleware applies rule per key (account, device or IP, chosen by keyFn). An empty key is refused.
func Middleware(l *Limiter, rule Rule, keyFn func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			who := keyFn(r)
			if who == "" {
				httpx.WriteProblem(w, http.StatusBadRequest, "unidentified-client", "Cannot identify client", "")
				return
			}
			d, err := l.Allow(r.Context(), rule.Name+":"+who, rule.Limit, rule.Window)
			if err != nil {
				slog.ErrorContext(r.Context(), "rate limiter unavailable", "rule", rule.Name, "fail_closed", rule.FailClosed, "err", err)
				if rule.FailClosed {
					httpx.WriteProblemRetry(w, http.StatusServiceUnavailable, "unavailable", "Temporarily unavailable", "", 5)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			if !d.Allowed {
				secs := int(math.Ceil(d.RetryAfter.Seconds()))
				httpx.WriteProblemRetry(w, http.StatusTooManyRequests, "rate-limited", "Too many requests",
					"retry after the indicated delay", max(secs, 1))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
