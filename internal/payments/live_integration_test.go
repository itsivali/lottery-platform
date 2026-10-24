//go:build integration

package payments_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/itsivali/lottery-platform/internal/payments"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
)

func TestLive_ResyncsEveryoneAfterTheListeningConnectionDrops(t *testing.T) {
	pool := testpg.Pool(t, "app_api")
	cfg := pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = "payment-events-test"
	l := payments.NewLive(func(ctx context.Context) (*pgx.Conn, error) { return pgx.ConnectConfig(ctx, cfg) }, payments.LiveConfig{})
	sub, _ := l.Subscribe("u1")
	go l.Run(t.Context())
	<-l.Ready()
	if ev := recv(t, sub); !ev.Resync {
		t.Fatalf("first event = %v, want the resync of the first LISTEN", ev)
	}

	if _, err := pool.Exec(context.Background(), `SELECT pg_notify('payment_events', 'u1:p1')`); err != nil {
		t.Fatal(err)
	}
	if ev := recv(t, sub); ev.PaymentID != "p1" {
		t.Fatalf("event = %v", ev)
	}

	// the login role (not the app role the pool switches to) may end the listener's backend
	admin, err := pgx.ConnectConfig(context.Background(), pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	if _, err := admin.Exec(context.Background(),
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = 'payment-events-test'`); err != nil {
		t.Fatal(err)
	}
	if ev := recv(t, sub); !ev.Resync {
		t.Fatalf("after the drop = %v, want a resync", ev)
	}
	if _, err := pool.Exec(context.Background(), `SELECT pg_notify('payment_events', 'u1:p2')`); err != nil {
		t.Fatal(err)
	}
	if ev := recv(t, sub); ev.PaymentID != "p2" {
		t.Fatalf("after reconnecting = %v", ev)
	}
}

func recv(t *testing.T, s *payments.Subscription) payments.Event {
	t.Helper()
	select {
	case ev := <-s.C:
		return ev
	case <-time.After(15 * time.Second):
		t.Fatal("no event within 15s")
		return payments.Event{}
	}
}
