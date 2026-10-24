package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/itsivali/lottery-platform/internal/game"
	"github.com/itsivali/lottery-platform/internal/game/fair"
	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/platform/httpx"
	"github.com/itsivali/lottery-platform/internal/rg"
)

type spinJSON struct {
	ChainID    string `json:"chain_id"`
	Commitment string `json:"commitment"`
	ClientSeed string `json:"client_seed"`
	Nonce      int64  `json:"nonce"`
	Slot       int    `json:"slot"`
	Lucky      bool   `json:"lucky"`
	Slots      int    `json:"slots"`
	LuckySlots int    `json:"lucky_slots"`
}

type playJSON struct {
	TicketID  string    `json:"ticket_id"`
	PlayDate  string    `json:"play_date"`
	Seq       int       `json:"seq"`
	PriceKES  int64     `json:"price_kes"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	Spin      spinJSON  `json:"spin"`
}

type todayJSON struct {
	PlayDate  string     `json:"play_date"`
	Plays     int        `json:"plays"`
	PlaysLeft int        `json:"plays_left"`
	Lucky     bool       `json:"lucky"`
	Chances   int        `json:"chances"`
	PriceKES  int64      `json:"price_kes"`
	Tickets   []playJSON `json:"tickets"`
}

func toPlayJSON(p game.Play) playJSON {
	s := p.Spin
	return playJSON{TicketID: p.TicketID, PlayDate: p.PlayDate.Format(time.DateOnly), Seq: p.Seq, PriceKES: p.PriceKES,
		Status: p.Status, CreatedAt: p.CreatedAt, Spin: spinJSON{ChainID: s.ChainID, Commitment: s.Commitment,
			ClientSeed: s.ClientSeed, Nonce: s.Nonce, Slot: s.Slot, Lucky: s.Lucky, Slots: s.Slots, LuckySlots: s.LuckySlots}}
}

func toTodayJSON(t game.Today) todayJSON {
	out := todayJSON{PlayDate: t.PlayDate.Format(time.DateOnly), Plays: t.Plays, PlaysLeft: t.PlaysLeft, Lucky: t.Lucky,
		Chances: t.Chances, PriceKES: t.PriceKES, Tickets: make([]playJSON, 0, len(t.Tickets))}
	for _, p := range t.Tickets {
		out.Tickets = append(out.Tickets, toPlayJSON(p))
	}
	return out
}

func (h *handlers) play(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if !h.limit(w, r, "play:user:"+p.UserID, 30, time.Hour) {
		return
	}
	if h.Game == nil {
		gameUnavailable(w)
		return
	}
	res, err := h.Game.Play(r.Context(), p.UserID, r.Header.Get("Idempotency-Key"))
	var re *rg.RestrictedError
	switch {
	case err == nil:
		status := http.StatusCreated
		if res.Replayed {
			status = http.StatusOK
		}
		httpx.WriteJSON(w, status, map[string]any{"play": toPlayJSON(res.Play), "today": toTodayJSON(res.Today), "balance_kes": res.BalanceKES})
	case errors.Is(err, game.ErrDailyCap):
		httpx.WriteProblem(w, http.StatusConflict, "daily-limit", "No plays left today",
			"you can play "+strconv.Itoa(game.DailyPlays)+" times a day; plays reset at midnight Nairobi time")
	case errors.Is(err, game.ErrInsufficientFunds):
		httpx.WriteProblem(w, http.StatusConflict, "insufficient-funds", "Your balance is below the price of a play", "top up first")
	case errors.Is(err, game.ErrInDebt):
		httpx.WriteProblem(w, http.StatusForbidden, "in-debt", "Play is paused while you owe money from a reversed top-up",
			"your next top-up repays it first")
	case errors.As(err, &re):
		httpx.WriteProblem(w, http.StatusForbidden, "restricted", "Your account is restricted", "")
	case errors.Is(err, game.ErrNoOdds):
		gameUnavailable(w)
	case errors.Is(err, identity.ErrKYCRequired), errors.Is(err, identity.ErrPhoneNotVerified), errors.Is(err, identity.ErrAccountSuspended):
		authError(w, err)
	default:
		internalError(w, err)
	}
}

func gameUnavailable(w http.ResponseWriter) {
	httpx.WriteProblemRetry(w, http.StatusServiceUnavailable, "game-unavailable", "Play is temporarily unavailable", "", 60)
}

func (h *handlers) playsToday(w http.ResponseWriter, r *http.Request) {
	if h.Game == nil {
		gameUnavailable(w)
		return
	}
	p, _ := principalFrom(r.Context())
	t, err := h.Game.Today(r.Context(), p.UserID)
	if err != nil {
		internalError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toTodayJSON(t))
}

type chainJSON struct {
	ID           string     `json:"id"`
	Commitment   string     `json:"commitment"`
	ClientSeed   string     `json:"client_seed"`
	NextNonce    int64      `json:"next_nonce"`
	RevealedSeed *string    `json:"revealed_seed"`
	CreatedAt    time.Time  `json:"created_at"`
	RevealedAt   *time.Time `json:"revealed_at"`
}

func toFairnessJSON(f game.Fairness) map[string]any {
	conv := func(c game.Chain) chainJSON {
		j := chainJSON{ID: c.ID, Commitment: c.Commitment, ClientSeed: c.ClientSeed, NextNonce: c.NextNonce, CreatedAt: c.CreatedAt, RevealedAt: c.RevealedAt}
		if c.RevealedSeed != "" {
			j.RevealedSeed = &c.RevealedSeed
		}
		return j
	}
	rotated := make([]chainJSON, 0, len(f.Rotated))
	for _, c := range f.Rotated {
		rotated = append(rotated, conv(c))
	}
	return map[string]any{"active": conv(f.Active), "rotated": rotated}
}

func (h *handlers) fairness(w http.ResponseWriter, r *http.Request) {
	if h.Game == nil {
		gameUnavailable(w)
		return
	}
	p, _ := principalFrom(r.Context())
	f, err := h.Game.Fairness(r.Context(), p.UserID)
	if err != nil {
		internalError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toFairnessJSON(f))
}

func (h *handlers) rotateSeed(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClientSeed string `json:"client_seed"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	p, _ := principalFrom(r.Context())
	if !h.limit(w, r, "rotate:user:"+p.UserID, 20, time.Hour) {
		return
	}
	if h.Game == nil {
		gameUnavailable(w)
		return
	}
	f, err := h.Game.Rotate(r.Context(), p.UserID, in.ClientSeed)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, toFairnessJSON(f))
	case errors.Is(err, fair.ErrBadClientSeed):
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid-client-seed", "Invalid client seed", "use 1 to 64 letters, digits, '-' or '_'")
	default:
		internalError(w, err)
	}
}
