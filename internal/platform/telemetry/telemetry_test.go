package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/itsivali/lottery-platform/internal/platform/telemetry"
)

func newTel(t *testing.T) (*telemetry.Telemetry, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tel, err := telemetry.New(context.Background(), telemetry.Config{ServiceName: "api-test", SpanProcessors: []trace.SpanProcessor{rec}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	return tel, rec
}

func hit(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, path, http.NoBody))
	return rec
}

func TestMiddleware_RecordsOneSpanPerRequestWithMethodAndStatus(t *testing.T) {
	tel, rec := newTel(t)
	h := tel.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	hit(h, http.MethodGet, "/v1/kyc")
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("%d spans, want 1", len(spans))
	}
	attrs := map[string]string{}
	for _, a := range spans[0].Attributes() {
		attrs[string(a.Key)] = a.Value.String()
	}
	if attrs["http.request.method"] != "GET" || attrs["http.response.status_code"] != "418" {
		t.Fatalf("attributes = %v", attrs)
	}
}

func TestMetricsEndpoint_ExposesRequestCountsInPrometheusFormat(t *testing.T) {
	tel, _ := newTel(t)
	h := tel.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	hit(h, http.MethodGet, "/x")
	hit(h, http.MethodGet, "/x")
	rec := hit(tel.MetricsHandler(), http.MethodGet, "/metrics")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "http_server_request_duration") {
		t.Fatalf("metrics output missing the request histogram:\n%s", body)
	}
}

func TestLogger_AddsTraceAndSpanIDsInsideASpan(t *testing.T) {
	tel, _ := newTel(t)
	var buf bytes.Buffer
	log := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(&buf, nil)))
	h := tel.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { log.InfoContext(r.Context(), "inside") }))
	hit(h, http.MethodGet, "/x")
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line %q: %v", buf.String(), err)
	}
	if len(line["trace_id"].(string)) != 32 || len(line["span_id"].(string)) != 16 {
		t.Fatalf("log line lacks trace correlation: %v", line)
	}
	buf.Reset()
	log.Info("outside")
	if strings.Contains(buf.String(), "trace_id") {
		t.Fatalf("trace_id logged outside any span: %s", buf.String())
	}
}

func TestNew_WithoutAnExporterStillWorks(t *testing.T) {
	tel, err := telemetry.New(context.Background(), telemetry.Config{ServiceName: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
