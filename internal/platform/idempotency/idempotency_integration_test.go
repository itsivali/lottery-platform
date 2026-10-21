//go:build integration

package idempotency_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itsivali/lottery-platform/internal/platform/idempotency"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
)

type fixture struct {
	srv   *httptest.Server
	calls atomic.Int64
	// status the handler answers with; set before a request
	status atomic.Int64
	hold   chan struct{} // when non-nil, the handler blocks until it is closed
}

func newFixture(t *testing.T, opts idempotency.Options) *fixture {
	t.Helper()
	pool := testpg.Pool(t, "app_api")
	f := &fixture{}
	f.status.Store(http.StatusCreated)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.calls.Add(1)
		if f.hold != nil {
			<-f.hold
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Secret-Internal", "must-not-be-replayed")
		w.WriteHeader(int(f.status.Load()))
		_, _ = w.Write([]byte(`{"call":` + string(rune('0'+n%10)) + `,"echo":"` + strings.TrimSpace(string(body)) + `"}`))
	})
	principal := func(r *http.Request) string { return r.Header.Get("X-Test-User") }
	mw := idempotency.Middleware(idempotency.NewStore(pool), principal, opts)
	f.srv = httptest.NewServer(mw(h))
	t.Cleanup(f.srv.Close)
	return f
}

// reply is the part of an HTTP response the tests inspect (the body is already read and closed).
type reply struct {
	StatusCode int
	Header     http.Header
}

func (f *fixture) do(t *testing.T, method, key, user, body string) (reply, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), method, f.srv.URL+"/v1/plays", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	req.Header.Set("X-Test-User", user)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return reply{StatusCode: resp.StatusCode, Header: resp.Header}, string(b)
}

func TestMiddleware_WriteWithoutKeyIsRejected(t *testing.T) {
	f := newFixture(t, idempotency.Options{})
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		resp, _ := f.do(t, m, "", "user:a", "{}")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s without key = %d, want 400", m, resp.StatusCode)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("handler ran without an idempotency key")
	}
}

func TestMiddleware_ReadsPassThroughWithoutKey(t *testing.T) {
	f := newFixture(t, idempotency.Options{})
	resp, _ := f.do(t, http.MethodGet, "", "user:a", "")
	if resp.StatusCode != http.StatusCreated || f.calls.Load() != 1 {
		t.Fatalf("GET status=%d calls=%d", resp.StatusCode, f.calls.Load())
	}
}

func TestMiddleware_MalformedKeyIsRejected(t *testing.T) {
	f := newFixture(t, idempotency.Options{})
	for _, k := range []string{strings.Repeat("k", 256), "bad key with spaces", "tab\tkey"} {
		if resp, _ := f.do(t, http.MethodPost, k, "user:a", "{}"); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("key %q = %d, want 400", k, resp.StatusCode)
		}
	}
}

func TestMiddleware_MissingPrincipalIsUnauthorised(t *testing.T) {
	f := newFixture(t, idempotency.Options{})
	if resp, _ := f.do(t, http.MethodPost, "k1", "", "{}"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestMiddleware_SameKeySameBodyReplaysTheStoredResponse(t *testing.T) {
	f := newFixture(t, idempotency.Options{})
	r1, b1 := f.do(t, http.MethodPost, "k1", "user:a", `{"x":1}`)
	r2, b2 := f.do(t, http.MethodPost, "k1", "user:a", `{"x":1}`)
	if f.calls.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", f.calls.Load())
	}
	if r1.StatusCode != r2.StatusCode || b1 != b2 {
		t.Fatalf("replay differs: %d %q vs %d %q", r1.StatusCode, b1, r2.StatusCode, b2)
	}
	if r2.Header.Get("Idempotent-Replay") != "true" || r1.Header.Get("Idempotent-Replay") != "" {
		t.Fatalf("replay header: first=%q second=%q", r1.Header.Get("Idempotent-Replay"), r2.Header.Get("Idempotent-Replay"))
	}
	if r2.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content-type not replayed: %q", r2.Header.Get("Content-Type"))
	}
	if r2.Header.Get("X-Secret-Internal") != "" {
		t.Fatal("arbitrary handler headers must not be stored and replayed")
	}
}

func TestMiddleware_SameKeyDifferentBodyIs422AndDoesNotRun(t *testing.T) {
	f := newFixture(t, idempotency.Options{})
	f.do(t, http.MethodPost, "k1", "user:a", `{"x":1}`)
	resp, _ := f.do(t, http.MethodPost, "k1", "user:a", `{"x":2}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", f.calls.Load())
	}
}

func TestMiddleware_SameKeyDifferentPathIs422(t *testing.T) {
	f := newFixture(t, idempotency.Options{})
	f.do(t, http.MethodPost, "k1", "user:a", `{}`)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, f.srv.URL+"/v1/other", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", "k1")
	req.Header.Set("X-Test-User", "user:a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

func TestMiddleware_KeysAreScopedPerPrincipal(t *testing.T) {
	f := newFixture(t, idempotency.Options{})
	f.do(t, http.MethodPost, "k1", "user:a", `{"x":1}`)
	resp, _ := f.do(t, http.MethodPost, "k1", "user:b", `{"x":1}`)
	if resp.StatusCode != http.StatusCreated || f.calls.Load() != 2 {
		t.Fatalf("status=%d calls=%d: user b must not see user a's key", resp.StatusCode, f.calls.Load())
	}
}

func TestMiddleware_ConcurrentRetriesRunTheHandlerOnce(t *testing.T) {
	f := newFixture(t, idempotency.Options{})
	f.hold = make(chan struct{})
	var wg sync.WaitGroup
	codes := make(chan int, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := f.do(t, http.MethodPost, "k1", "user:a", `{"x":1}`)
			codes <- resp.StatusCode
		}()
	}
	// wait until the first request is inside the handler and the rest have been turned away, then release it
	deadline := time.Now().Add(10 * time.Second)         //nolint:forbidigo // test polling deadline
	for len(codes) < 19 && time.Now().Before(deadline) { //nolint:forbidigo // test polling deadline
		time.Sleep(5 * time.Millisecond) //nolint:forbidigo // polling helper
	}
	close(f.hold)
	wg.Wait()
	close(codes)
	if f.calls.Load() != 1 {
		t.Fatalf("handler ran %d times, want exactly 1", f.calls.Load())
	}
	var created, conflict int
	for c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if created != 1 || conflict != 19 {
		t.Fatalf("created=%d conflict=%d, want 1/19", created, conflict)
	}
}

func TestMiddleware_ServerErrorsAreNotCachedSoTheClientCanRetry(t *testing.T) {
	f := newFixture(t, idempotency.Options{})
	f.status.Store(http.StatusInternalServerError)
	if resp, _ := f.do(t, http.MethodPost, "k1", "user:a", `{}`); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("first status = %d", resp.StatusCode)
	}
	f.status.Store(http.StatusCreated)
	resp, _ := f.do(t, http.MethodPost, "k1", "user:a", `{}`)
	if resp.StatusCode != http.StatusCreated || f.calls.Load() != 2 {
		t.Fatalf("retry status=%d calls=%d, want 201/2", resp.StatusCode, f.calls.Load())
	}
}

func TestMiddleware_ClientErrorsAreCachedAndReplayed(t *testing.T) {
	f := newFixture(t, idempotency.Options{})
	f.status.Store(http.StatusConflict)
	r1, _ := f.do(t, http.MethodPost, "k1", "user:a", `{}`)
	f.status.Store(http.StatusCreated)
	r2, _ := f.do(t, http.MethodPost, "k1", "user:a", `{}`)
	if r1.StatusCode != http.StatusConflict || r2.StatusCode != http.StatusConflict || f.calls.Load() != 1 {
		t.Fatalf("statuses %d/%d calls %d", r1.StatusCode, r2.StatusCode, f.calls.Load())
	}
}

func TestMiddleware_ExpiredKeyCanBeReused(t *testing.T) {
	f := newFixture(t, idempotency.Options{TTL: 200 * time.Millisecond})
	f.do(t, http.MethodPost, "k1", "user:a", `{"x":1}`)
	deadline := time.Now().Add(10 * time.Second) //nolint:forbidigo // test polling deadline
	for {
		resp, _ := f.do(t, http.MethodPost, "k1", "user:a", `{"x":2}`) // different body: only legal once expired
		if resp.StatusCode == http.StatusCreated {
			break
		}
		if time.Now().After(deadline) { //nolint:forbidigo // test polling deadline
			t.Fatalf("expired key never became reusable (last status %d)", resp.StatusCode)
		}
		time.Sleep(50 * time.Millisecond) //nolint:forbidigo // polling helper
	}
}

func TestMiddleware_AbandonedInFlightKeyIsTakenOverAfterTheStaleWindow(t *testing.T) {
	pool := testpg.Pool(t, "app_api")
	st := idempotency.NewStore(pool)
	ctx := context.Background()
	// simulate a crashed request: key claimed, never completed
	if _, err := st.Claim(ctx, "user:a", "k1", []byte("h"), time.Hour, time.Minute); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second) //nolint:forbidigo // test polling deadline
	for {
		res, err := st.Claim(ctx, "user:a", "k1", []byte("h"), time.Hour, 100*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome == idempotency.Claimed {
			return
		}
		if time.Now().After(deadline) { //nolint:forbidigo // test polling deadline
			t.Fatalf("stale claim never taken over, last outcome %v", res.Outcome)
		}
		time.Sleep(50 * time.Millisecond) //nolint:forbidigo // polling helper
	}
}
