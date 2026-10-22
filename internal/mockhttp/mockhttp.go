// Package mockhttp is a tiny canned-response server for the local placeholder mocks (kyc, drand). Mocks that need
// behaviour, such as Daraja (internal/payments/darajamock), have their own packages.
package mockhttp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// Handler serves /healthz plus the given fixed JSON bodies by path.
func Handler(name string, routes map[string]string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(name + " ok"))
	})
	for p, body := range routes {
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
	}
	return mux
}

// LogRequests wraps h and reports "METHOD path body" for every request except health checks, so a developer can see
// what the api sent to a local mock; it is for local mocks only.
func LogRequests(h http.Handler, logf func(string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
			r.Body = io.NopCloser(bytes.NewReader(body))
			logf(r.Method + " " + r.URL.Path + " " + string(body))
		}
		h.ServeHTTP(w, r)
	})
}

// RoutesFromJSON parses a {"path":"body"} object (the MOCK_ROUTES environment variable); empty means no routes.
func RoutesFromJSON(s string) (map[string]string, error) {
	routes := map[string]string{}
	if s == "" {
		return routes, nil
	}
	return routes, json.Unmarshal([]byte(s), &routes)
}
