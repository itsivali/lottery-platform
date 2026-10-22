package httpx_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/itsivali/lottery-platform/internal/platform/httpx"
)

func TestLogClientIPHeaders_LogsOnlyAddressHeadersAndPassesTheRequestOn(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	served := false
	h := httpx.LogClientIPHeaders(log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusTeapot)
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/auth/login?secret=1", strings.NewReader(`{"password":"hunter2"}`))
	req.RemoteAddr = "10.0.0.7:5555"
	req.Header.Set("X-Forwarded-For", "9.9.9.9, 1.2.3.4")
	req.Header.Set("CF-Connecting-IP", "1.2.3.4")
	req.Header.Set("True-Client-IP", "1.2.3.4")
	req.Header.Set("Authorization", "Bearer sensitive-token")
	req.Header.Set("Cookie", "session=abc")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !served || rec.Code != http.StatusTeapot {
		t.Fatalf("request not passed on: served=%v code=%d", served, rec.Code)
	}
	out := buf.String()
	for _, want := range []string{`"remote_addr":"10.0.0.7:5555"`, `"x-forwarded-for":"9.9.9.9, 1.2.3.4"`, `"cf-connecting-ip":"1.2.3.4"`, `"true-client-ip":"1.2.3.4"`, `"path":"/v1/auth/login"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s: %s", want, out)
		}
	}
	for _, leak := range []string{"hunter2", "sensitive-token", "session=abc", "secret=1"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaks %q: %s", leak, out)
		}
	}
}
