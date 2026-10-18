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
	h := mockhttp.Handler(name, nil)
	log.Fatal((&http.Server{Addr: ":8080", Handler: h, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe())
}
