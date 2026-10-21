package httpx_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/itsivali/lottery-platform/internal/platform/httpx"
)

func TestWriteProblem_IsRFC7807(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.WriteProblem(rec, http.StatusUnprocessableEntity, "idempotency-key-reuse", "Idempotency key reused", "different body")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type = %q", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != float64(422) || body["title"] != "Idempotency key reused" ||
		body["detail"] != "different body" || body["type"] != "https://lottery.example/problems/idempotency-key-reuse" {
		t.Fatalf("body = %v", body)
	}
}

func TestWriteProblem_RetryAfterHeaderIsOptional(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.WriteProblemRetry(rec, http.StatusTooManyRequests, "rate-limited", "Slow down", "", 7)
	if got := rec.Header().Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After = %q, want 7", got)
	}
}
