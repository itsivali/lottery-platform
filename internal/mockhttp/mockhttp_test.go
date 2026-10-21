package mockhttp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/itsivali/lottery-platform/internal/mockhttp"
)

func TestHandler_HealthzAndNamedRoutes(t *testing.T) {
	h := mockhttp.Handler("kyc", map[string]string{"/v1/verify": `{"match":"exact"}`})
	tests := []struct {
		path string
		code int
	}{
		{"/healthz", 200},
		{"/v1/verify", 200},
		{"/unknown", 404},
	}
	for _, tc := range tests {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, tc.path, nil))
		if rec.Code != tc.code {
			t.Errorf("%s = %d, want %d", tc.path, rec.Code, tc.code)
		}
	}
}

func TestLogRequests_LogsMethodPathAndBodyButNotHealthChecks(t *testing.T) {
	var lines []string
	h := mockhttp.LogRequests(mockhttp.Handler("sms", map[string]string{"/send": `{}`}), func(s string) { lines = append(lines, s) })
	post := func(path, body string) {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body)))
	}
	post("/send", `{"to":"254712345678","message":"Your code is 123456"}`)
	post("/healthz", "")
	if len(lines) != 1 || !strings.Contains(lines[0], "POST /send") || !strings.Contains(lines[0], "Your code is 123456") {
		t.Fatalf("lines = %q", lines)
	}
}

func TestRoutesFromJSON(t *testing.T) {
	m, err := mockhttp.RoutesFromJSON(`{"/send":"{}","/verify":"{\"found\":true}"}`)
	if err != nil || len(m) != 2 || m["/verify"] != `{"found":true}` {
		t.Fatalf("%v %v", m, err)
	}
	if m, err := mockhttp.RoutesFromJSON(""); err != nil || len(m) != 0 {
		t.Fatalf("empty: %v %v", m, err)
	}
	if _, err := mockhttp.RoutesFromJSON("{not json"); err == nil {
		t.Fatal("bad json accepted")
	}
}
