// Command daraja is the local Daraja (M-Pesa) mock: OAuth, STK Push, STK query and callbacks, with fault injection
// through POST /__mock/fault and manual completion through POST /__mock/complete/{checkoutID}. See internal/payments/darajamock.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/itsivali/lottery-platform/internal/payments/darajamock"
)

func main() {
	m := darajamock.New(darajamock.Options{
		ConsumerKey: env("MOCK_CONSUMER_KEY", "local"), ConsumerSecret: env("MOCK_CONSUMER_SECRET", "local"),
		ShortCode: env("MOCK_SHORTCODE", "174379"), Passkey: env("MOCK_PASSKEY", "local"),
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	})
	// MOCK_AUTO_COMPLETE (a duration such as 2s) pays every push after that delay; empty leaves pushes pending.
	if v := os.Getenv("MOCK_AUTO_COMPLETE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("MOCK_AUTO_COMPLETE: %v", err)
		}
		m.AutoComplete(context.Background(), d)
	}
	log.Fatal((&http.Server{Addr: ":8080", Handler: m, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe())
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
