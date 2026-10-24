//go:build integration

package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/wallet"
)

// canPlay makes the session's account verified with a proved phone and credits gross net of the 5% excise.
func (e *env) canPlay(t *testing.T, s session, gross int64) {
	t.Helper()
	e.verified(t, s)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE identity.users SET msisdn_verified_at = now() WHERE msisdn = $1`, s.msisdn); err != nil {
		t.Fatal(err)
	}
	ls := ledger.NewStore()
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		var user, paymentID string
		if err := tx.QueryRow(ctx, `SELECT id::text, platform.uuid_v7()::text FROM identity.users WHERE msisdn = $1`, s.msisdn).Scan(&user, &paymentID); err != nil {
			return err
		}
		rule, err := ls.ActiveRule(ctx, tx, "deposit_excise", time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
		if err != nil {
			return err
		}
		p, err := ledger.Deposit(paymentID, user, gross, rule, true)
		if err != nil {
			return err
		}
		_, err = wallet.NewService(ls).Apply(ctx, tx, p)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAPI_PlayJourneyFromTheFirstPlayToTheDailyLimit(t *testing.T) {
	e := setup(t)
	s := e.signIn(t)
	e.canPlay(t, s, 1000) // credited 950

	f := e.do(t, http.MethodGet, "/v1/fairness", s.access, nil)
	active, _ := f.Body["active"].(map[string]any)
	if f.Code != http.StatusOK || len(active["commitment"].(string)) != 64 {
		t.Fatalf("fairness: %d %s", f.Code, f.Raw)
	}
	r := e.doKey(t, http.MethodPost, "/v1/plays", s.access, "play-1", nil)
	if r.Code != http.StatusCreated || r.Body["balance_kes"] != float64(850) {
		t.Fatalf("play: %d %s", r.Code, r.Raw)
	}
	play := r.Body["play"].(map[string]any)
	spin := play["spin"].(map[string]any)
	if play["price_kes"] != float64(100) || spin["commitment"] != active["commitment"] || spin["nonce"] != float64(0) {
		t.Fatalf("play = %v", play)
	}
	// the same key is the same play, charged once
	if again := e.doKey(t, http.MethodPost, "/v1/plays", s.access, "play-1", nil); again.Code != http.StatusCreated ||
		again.Body["play"].(map[string]any)["ticket_id"] != play["ticket_id"] {
		t.Fatalf("replay: %d %s", again.Code, again.Raw)
	}
	for i := 2; i <= 4; i++ {
		if r := e.do(t, http.MethodPost, "/v1/plays", s.access, nil); r.Code != http.StatusCreated {
			t.Fatalf("play %d: %d %s", i, r.Code, r.Raw)
		}
	}
	if r := e.do(t, http.MethodPost, "/v1/plays", s.access, nil); r.Code != http.StatusConflict || !hasType(r, "daily-limit") {
		t.Fatalf("fifth play: %d %s", r.Code, r.Raw)
	}
	today := e.do(t, http.MethodGet, "/v1/plays/today", s.access, nil)
	if today.Code != http.StatusOK || today.Body["plays"] != float64(4) || today.Body["plays_left"] != float64(0) ||
		len(today.Body["tickets"].([]any)) != 4 {
		t.Fatalf("today: %d %s", today.Code, today.Raw)
	}
	chances := today.Body["chances"].(float64)
	if (today.Body["lucky"] == true && chances != 10) || (today.Body["lucky"] == false && chances != 5) {
		t.Fatalf("chances %v lucky %v", chances, today.Body["lucky"])
	}

	rot := e.do(t, http.MethodPost, "/v1/fairness/rotate", s.access, map[string]any{"client_seed": "mine"})
	rotated, _ := rot.Body["rotated"].([]any)
	if rot.Code != http.StatusOK || len(rotated) != 1 || rotated[0].(map[string]any)["revealed_seed"] == nil ||
		rot.Body["active"].(map[string]any)["client_seed"] != "mine" {
		t.Fatalf("rotate: %d %s", rot.Code, rot.Raw)
	}
}

func TestAPI_PlayErrorMapping(t *testing.T) {
	e := setup(t)
	noKYC := e.signIn(t)
	if r := e.do(t, http.MethodPost, "/v1/plays", noKYC.access, nil); r.Code != http.StatusForbidden || !hasType(r, "kyc-required") {
		t.Fatalf("no kyc: %d %s", r.Code, r.Raw)
	}
	noPhone := e.signIn(t)
	e.verified(t, noPhone)
	if r := e.do(t, http.MethodPost, "/v1/plays", noPhone.access, nil); r.Code != http.StatusForbidden || !hasType(r, "phone-not-verified") {
		t.Fatalf("no phone: %d %s", r.Code, r.Raw)
	}
	broke := e.signIn(t)
	e.canPlay(t, broke, 50)
	if r := e.do(t, http.MethodPost, "/v1/plays", broke.access, nil); r.Code != http.StatusConflict || !hasType(r, "insufficient-funds") {
		t.Fatalf("broke: %d %s", r.Code, r.Raw)
	}
	if r := e.do(t, http.MethodPost, "/v1/fairness/rotate", broke.access, map[string]any{"client_seed": "no spaces"}); r.Code != http.StatusBadRequest || !hasType(r, "invalid-client-seed") {
		t.Fatalf("bad seed: %d %s", r.Code, r.Raw)
	}
}

func hasType(r resp, slug string) bool {
	typ, _ := r.Body["type"].(string)
	return strings.HasSuffix(typ, "/"+slug)
}
