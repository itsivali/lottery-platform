package mockhttp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
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
