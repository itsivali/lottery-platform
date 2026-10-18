// Package mockhttp is a tiny canned-response server used for the Phase 0 local mocks.
// Real fault-injecting mocks replace it in later phases.
package mockhttp

import "net/http"

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
