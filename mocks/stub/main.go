// Command stub serves a named placeholder mock (kyc, sms, drand) for local Compose.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/itsivali/lottery-platform/internal/mockhttp"
)

func main() {
	name := os.Getenv("MOCK_NAME")
	if name == "" {
		name = "stub"
	}
	routes, err := mockhttp.RoutesFromJSON(os.Getenv("MOCK_ROUTES"))
	if err != nil {
		log.Fatalf("MOCK_ROUTES: %v", err)
	}
	// %q: request bodies are untrusted, so each is logged as one quoted, escaped line
	h := mockhttp.LogRequests(mockhttp.Handler(name, routes), func(line string) { log.Printf("%s: %q", name, line) }) //nolint:gosec // local dev mock; the value is %q-escaped
	log.Fatal((&http.Server{Addr: ":8080", Handler: h, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe())
}
