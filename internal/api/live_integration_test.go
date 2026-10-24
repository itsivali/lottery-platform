//go:build integration

package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/itsivali/lottery-platform/internal/api"
)

type sseEvent struct {
	name string
	data map[string]any
}

// stream is an open GET /v1/payments/events; next returns its events in order.
type stream struct {
	events chan sseEvent
	cancel context.CancelFunc
}

func (e *env) openStream(t *testing.T, token string) (*stream, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.srv.URL+"/v1/payments/events", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	r, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed below, or by the reader goroutine when the stream ends
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK {
		_ = r.Body.Close()
		cancel()
		return nil, r.StatusCode
	}
	if ct := r.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	s := &stream{events: make(chan sseEvent, 64), cancel: cancel}
	t.Cleanup(cancel)
	go func() {
		defer close(s.events)
		defer func() { _ = r.Body.Close() }()
		sc := bufio.NewScanner(r.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.name != "" {
					s.events <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.data)
			}
		}
	}()
	return s, http.StatusOK
}

func (s *stream) next(t *testing.T) (sseEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		return ev, ok
	case <-time.After(10 * time.Second):
		t.Fatal("no event within 10s")
		return sseEvent{}, false
	}
}

func (s *stream) expect(t *testing.T, name string) sseEvent {
	t.Helper()
	ev, ok := s.next(t)
	if !ok || ev.name != name {
		t.Fatalf("got %q (open=%v) %v, want %q", ev.name, ok, ev.data, name)
	}
	return ev
}

func (e *env) waitLive(t *testing.T) {
	t.Helper()
	select {
	case <-e.live.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("payment events listener not ready")
	}
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (e *env) topUp(t *testing.T, s session, amount int) string {
	t.Helper()
	r := e.do(t, http.MethodPost, "/v1/payments/topups", s.access, map[string]any{"amount_kes": amount})
	if r.Code != http.StatusAccepted {
		t.Fatalf("top-up: %d %s", r.Code, r.Raw)
	}
	return r.Body["id"].(string)
}

func TestAPI_PaymentStreamStartsFromTheSnapshotAndSendsEachVisibleChange(t *testing.T) {
	e := setup(t)
	e.waitLive(t)
	a, b := e.signIn(t), e.signIn(t)
	e.verified(t, a)
	e.verified(t, b)
	before := e.topUp(t, a, 150) // started before the stream: must be in the snapshot

	s, code := e.openStream(t, a.access)
	if code != http.StatusOK {
		t.Fatalf("open: %d", code)
	}
	if ev := s.expect(t, "payment"); ev.data["id"] != before || ev.data["status"] != "pending" {
		t.Fatalf("snapshot = %v", ev.data)
	}
	s.expect(t, "ready")

	// another player's change never reaches this stream
	other := e.topUp(t, b, 200)
	e.exec(t, `UPDATE payments.payments SET status = 'failed' WHERE id = $1`, other)
	// an internal step the player cannot see (still pending) sends nothing
	e.exec(t, `UPDATE payments.payments SET status = 'stk_sent', checkout_request_id = 'ws_CO_1' WHERE id = $1`, before)
	e.exec(t, `UPDATE payments.payments SET status = 'credited', amount_confirmed_kes = 140, mpesa_receipt = 'TJK1ABCDEF' WHERE id = $1`, before)
	ev := s.expect(t, "payment")
	if ev.data["id"] != before || ev.data["status"] != "credited" || ev.data["amount_confirmed_kes"] != float64(140) {
		t.Fatalf("credit event = %v", ev.data)
	}

	// a new top-up (a paybill credit inserts a payment the same way) is announced as it is created
	next := e.topUp(t, a, 50)
	if ev := s.expect(t, "payment"); ev.data["id"] != next || ev.data["status"] != "pending" {
		t.Fatalf("new payment event = %v", ev.data)
	}

	// a rolled-back change is never announced
	tx, _ := e.pool.Begin(context.Background())
	_, _ = tx.Exec(context.Background(), `UPDATE payments.payments SET status = 'failed' WHERE id = $1`, next)
	_ = tx.Rollback(context.Background())
	e.exec(t, `UPDATE payments.payments SET status = 'cancelled' WHERE id = $1`, next)
	if ev := s.expect(t, "payment"); ev.data["id"] != next || ev.data["status"] != "failed" {
		t.Fatalf("after rollback = %v", ev.data)
	}
}

func TestAPI_PaymentStreamNeedsATokenAndEndsWhenTheSessionIsRevoked(t *testing.T) {
	defer api.SetLiveHeartbeat(200 * time.Millisecond)()
	e := setup(t)
	e.waitLive(t)
	for _, tok := range []string{"", "abc.def.ghi"} {
		if _, code := e.openStream(t, tok); code != http.StatusUnauthorized {
			t.Fatalf("token %q: %d", tok, code)
		}
	}
	a := e.signIn(t)
	s, _ := e.openStream(t, a.access)
	s.expect(t, "ready")
	if r := e.do(t, http.MethodPost, "/v1/auth/logout", a.access, nil); r.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", r.Code)
	}
	s.expect(t, "expired")
	if _, open := s.next(t); open {
		t.Fatal("stream still open after expiry")
	}
}

func TestAPI_PaymentStreamsAreCappedPerPlayerAndEndAtShutdown(t *testing.T) {
	e := setup(t)
	e.waitLive(t)
	a := e.signIn(t)
	var open []*stream
	for range 3 {
		s, code := e.openStream(t, a.access)
		if code != http.StatusOK {
			t.Fatalf("stream %d: %d", len(open)+1, code)
		}
		s.expect(t, "ready")
		open = append(open, s)
	}
	if _, code := e.openStream(t, a.access); code != http.StatusTooManyRequests {
		t.Fatalf("4th stream: %d, want 429", code)
	}
	open[0].cancel() // a closed stream frees its place
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, code := e.openStream(t, a.access)
		if code == http.StatusOK {
			s.expect(t, "ready")
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("place not freed: %d", code)
		}
		time.Sleep(50 * time.Millisecond)
	}

	e.live.Close()
	for _, s := range open[1:] {
		for {
			if _, ok := s.next(t); !ok {
				break
			}
		}
	}
	if _, code := e.openStream(t, a.access); code != http.StatusServiceUnavailable {
		t.Fatalf("after shutdown: %d, want 503", code)
	}
}
