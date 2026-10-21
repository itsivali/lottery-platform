package notifications_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/itsivali/lottery-platform/internal/notifications"
)

func TestHTTPSMS_PostsTheMessageWithBearerAuth(t *testing.T) {
	var got struct{ To, Message string }
	var auth, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	s := notifications.NewHTTPSMS(srv.URL, "secret-key", srv.Client())
	if err := s.Send(context.Background(), "254712345678", "Your code is 123456"); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer secret-key" || ctype != "application/json" || got.To != "254712345678" || got.Message != "Your code is 123456" {
		t.Fatalf("auth=%q type=%q body=%+v", auth, ctype, got)
	}
}

func TestHTTPSMS_FailuresNeverLeakTheMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad: your code is 123456", http.StatusBadGateway) // a gateway echoing the message back
	}))
	defer srv.Close()
	err := notifications.NewHTTPSMS(srv.URL, "k", srv.Client()).Send(context.Background(), "254712345678", "Your code is 123456")
	if err == nil || strings.Contains(err.Error(), "123456") {
		t.Fatalf("err = %v: must fail without echoing the message or the gateway body", err)
	}
}

func TestHTTPSMS_HonoursContextAndTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := notifications.NewHTTPSMS(srv.URL, "k", srv.Client()).Send(ctx, "254712345678", "x"); err == nil {
		t.Fatal("hung gateway did not fail the send")
	}
}

func TestUnavailableSMS_FailsClosed(t *testing.T) {
	if err := (notifications.UnavailableSMS{}).Send(context.Background(), "254712345678", "x"); !errors.Is(err, notifications.ErrSMSUnavailable) {
		t.Fatalf("err = %v", err)
	}
}
