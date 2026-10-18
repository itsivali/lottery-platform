package httpx_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/itsivali/lottery-platform/internal/platform/httpx"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
	return rec
}

func TestHealthz_Returns200EvenWhenDatabaseIsDown(t *testing.T) {
	h := httpx.NewRouter(fakePinger{err: errors.New("db down")})
	if got := get(t, h, "/healthz").Code; got != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", got)
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"postgres up", nil, http.StatusOK},
		{"postgres down", errors.New("connection refused"), http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := httpx.NewRouter(fakePinger{err: tc.err})
			if got := get(t, h, "/readyz").Code; got != tc.want {
				t.Fatalf("GET /readyz = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSecurityHeaders_PresentOnEveryResponse(t *testing.T) {
	h := httpx.NewRouter(fakePinger{})
	for _, p := range []string{"/healthz", "/readyz", "/nope"} {
		rec := get(t, h, p)
		for _, k := range []string{"Strict-Transport-Security", "X-Content-Type-Options", "X-Frame-Options"} {
			if rec.Header().Get(k) == "" {
				t.Errorf("%s: missing header %s", p, k)
			}
		}
	}
}
