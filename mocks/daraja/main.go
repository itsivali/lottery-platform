// Command daraja is the local Daraja mock (Phase 0: health + OAuth only; fault injection comes in Phase 2).
package main

import (
	"log"
	"net/http"
	"time"

	"github.com/itsivali/lottery-platform/internal/mockhttp"
)

func main() {
	h := mockhttp.Handler("daraja-mock", map[string]string{
		"/oauth/v1/generate": `{"access_token":"mock-token","expires_in":"3599"}`,
	})
	log.Fatal((&http.Server{Addr: ":8080", Handler: h, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe())
}
