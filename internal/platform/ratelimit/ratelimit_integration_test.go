//go:build integration

package ratelimit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itsivali/lottery-platform/internal/platform/ratelimit"
	"github.com/itsivali/lottery-platform/internal/platform/testvalkey"
)

func TestAllow_PermitsExactlyLimitRequestsPerWindow(t *testing.T) {
	l := ratelimit.New(testvalkey.Start(t).Client(t))
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		d, err := l.Allow(ctx, "otp:254712345678", 5, time.Minute)
		if err != nil || !d.Allowed {
			t.Fatalf("request %d: %+v, %v; want allowed", i, d, err)
		}
		if d.Remaining != 5-i {
			t.Fatalf("request %d: remaining = %d, want %d", i, d.Remaining, 5-i)
		}
	}
	d, err := l.Allow(ctx, "otp:254712345678", 5, time.Minute)
	if err != nil || d.Allowed {
		t.Fatalf("6th request: %+v, %v; want refused", d, err)
	}
	if d.RetryAfter <= 0 || d.RetryAfter > time.Minute {
		t.Fatalf("RetryAfter = %v", d.RetryAfter)
	}
}

func TestAllow_KeysAreIndependent(t *testing.T) {
	l := ratelimit.New(testvalkey.Start(t).Client(t))
	ctx := context.Background()
	if _, err := l.Allow(ctx, "a", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if d, _ := l.Allow(ctx, "a", 1, time.Minute); d.Allowed {
		t.Fatal("key a should be exhausted")
	}
	if d, _ := l.Allow(ctx, "b", 1, time.Minute); !d.Allowed {
		t.Fatal("key b must be unaffected by key a")
	}
}

func TestAllow_WindowSlides(t *testing.T) {
	l := ratelimit.New(testvalkey.Start(t).Client(t))
	ctx := context.Background()
	// The window must comfortably exceed the time between the first two calls: the first one also opens the
	// connection and loads the script, which under load took longer than the 300ms window this test used to have,
	// so the second request landed in a fresh window and was (correctly) allowed.
	const window = 2 * time.Second
	if d, err := l.Allow(ctx, "k", 1, window); err != nil || !d.Allowed {
		t.Fatalf("first request refused: %v", err)
	}
	if d, err := l.Allow(ctx, "k", 1, window); err != nil || d.Allowed {
		t.Fatalf("second request inside the window allowed (err %v)", err)
	}
	deadline := time.Now().Add(10 * time.Second) //nolint:forbidigo // test polling deadline
	for {
		if d, _ := l.Allow(ctx, "k", 1, window); d.Allowed {
			return
		}
		if time.Now().After(deadline) { //nolint:forbidigo // test polling deadline
			t.Fatal("window never slid")
		}
		time.Sleep(25 * time.Millisecond) //nolint:forbidigo // polling helper
	}
}

func TestAllow_ConcurrentCallersNeverExceedTheLimit(t *testing.T) {
	l := ratelimit.New(testvalkey.Start(t).Client(t))
	ctx := context.Background()
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, err := l.Allow(ctx, "burst", 25, time.Minute); err == nil && d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 25 {
		t.Fatalf("allowed = %d, want exactly 25", allowed.Load())
	}
}

func handler(hits *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); w.WriteHeader(http.StatusOK) })
}

func do(h http.Handler, method string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), method, "/v1/plays", nil)
	req.Header.Set("X-Who", "user:a")
	h.ServeHTTP(rec, req)
	return rec
}

func TestMiddleware_Answers429WithRetryAfterWhenExceeded(t *testing.T) {
	l := ratelimit.New(testvalkey.Start(t).Client(t))
	var hits atomic.Int64
	h := ratelimit.Middleware(l, ratelimit.Rule{Name: "play", Limit: 2, Window: time.Minute, FailClosed: true},
		func(r *http.Request) string { return r.Header.Get("X-Who") })(handler(&hits))
	do(h, http.MethodPost)
	do(h, http.MethodPost)
	rec := do(h, http.MethodPost)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	if hits.Load() != 2 {
		t.Fatalf("handler hits = %d, want 2", hits.Load())
	}
}

func TestMiddleware_FailsClosedWhenValkeyIsDownOnMoneyRoutes(t *testing.T) {
	srv := testvalkey.Start(t)
	l := ratelimit.New(srv.Client(t))
	var hits atomic.Int64
	h := ratelimit.Middleware(l, ratelimit.Rule{Name: "topup", Limit: 10, Window: time.Minute, FailClosed: true},
		func(r *http.Request) string { return r.Header.Get("X-Who") })(handler(&hits))
	srv.Stop()
	rec := do(h, http.MethodPost)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if hits.Load() != 0 {
		t.Fatal("money route ran while the limiter was unavailable")
	}
}

func TestMiddleware_FailsOpenWhenValkeyIsDownOnReadRoutes(t *testing.T) {
	srv := testvalkey.Start(t)
	l := ratelimit.New(srv.Client(t))
	var hits atomic.Int64
	h := ratelimit.Middleware(l, ratelimit.Rule{Name: "read", Limit: 10, Window: time.Minute, FailClosed: false},
		func(r *http.Request) string { return r.Header.Get("X-Who") })(handler(&hits))
	srv.Stop()
	if rec := do(h, http.MethodGet); rec.Code != http.StatusOK || hits.Load() != 1 {
		t.Fatalf("status=%d hits=%d, want 200/1", rec.Code, hits.Load())
	}
}

func TestMiddleware_EmptyKeyIsRefused(t *testing.T) {
	l := ratelimit.New(testvalkey.Start(t).Client(t))
	var hits atomic.Int64
	h := ratelimit.Middleware(l, ratelimit.Rule{Name: "play", Limit: 2, Window: time.Minute, FailClosed: true},
		func(*http.Request) string { return "" })(handler(&hits))
	if rec := do(h, http.MethodPost); rec.Code != http.StatusBadRequest || hits.Load() != 0 {
		t.Fatalf("status=%d hits=%d, want 400/0", rec.Code, hits.Load())
	}
}
