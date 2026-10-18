//go:build integration

package httpx_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/itsivali/lottery-platform/internal/platform/db"
	"github.com/itsivali/lottery-platform/internal/platform/httpx"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
)

func TestReadyz_RealPostgresUpThenDown(t *testing.T) {
	dsn, stop := testpg.Start(t)
	pool, err := db.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	h := httpx.NewRouter(pool)

	if got := get(t, h, "/readyz").Code; got != http.StatusOK {
		t.Fatalf("with Postgres up: /readyz = %d, want 200", got)
	}
	stop()
	if got := get(t, h, "/readyz").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("with Postgres down: /readyz = %d, want 503", got)
	}
}
