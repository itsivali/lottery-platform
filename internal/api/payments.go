package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/payments"
	"github.com/itsivali/lottery-platform/internal/payments/daraja"
	"github.com/itsivali/lottery-platform/internal/platform/httpx"
	"github.com/itsivali/lottery-platform/internal/rg"
)

type topUpJSON struct {
	ID                 string    `json:"id"`
	Status             string    `json:"status"`
	AmountRequestedKES int64     `json:"amount_requested_kes"`
	AmountConfirmedKES *int64    `json:"amount_confirmed_kes"`
	CreatedAt          time.Time `json:"created_at"`
}

func toTopUpJSON(t payments.TopUp) topUpJSON {
	return topUpJSON{ID: t.ID, Status: t.Status, AmountRequestedKES: t.AmountRequestedKES, AmountConfirmedKES: t.AmountConfirmedKES, CreatedAt: t.CreatedAt}
}

func (h *handlers) startTopUp(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AmountKES int64 `json:"amount_kes"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	p, _ := principalFrom(r.Context())
	if !h.limit(w, r, "topup:user:"+p.UserID, 20, time.Hour) {
		return
	}
	if h.Payments == nil {
		paymentsUnavailable(w)
		return
	}
	t, err := h.Payments.StartTopUp(r.Context(), p.UserID, in.AmountKES, r.Header.Get("Idempotency-Key"))
	var le *rg.LimitError
	var re *rg.RestrictedError
	switch {
	case err == nil:
		// the prompt is sent in the background; the app polls GET /v1/payments/{id}
		httpx.WriteJSON(w, http.StatusAccepted, toTopUpJSON(t))
	case errors.Is(err, payments.ErrInvalidAmount):
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid-amount", "Invalid amount",
			"top-ups are between KES "+strconv.FormatInt(payments.MinTopUpKES, 10)+" and KES "+strconv.Itoa(daraja.MaxSTKAmountKES))
	case errors.Is(err, payments.ErrUnavailable):
		paymentsUnavailable(w)
	case errors.As(err, &le):
		httpx.WriteProblem(w, http.StatusForbidden, "deposit-limit", "This top-up would exceed your "+string(le.Period)+" deposit limit",
			"you can still deposit KES "+strconv.FormatInt(le.Remaining, 10)+" in this period")
	case errors.As(err, &re):
		httpx.WriteProblem(w, http.StatusForbidden, "restricted", "Your account is restricted", "")
	case errors.Is(err, identity.ErrKYCRequired), errors.Is(err, identity.ErrAccountSuspended):
		authError(w, err)
	default:
		internalError(w, err)
	}
}

func paymentsUnavailable(w http.ResponseWriter) {
	httpx.WriteProblemRetry(w, http.StatusServiceUnavailable, "payments-unavailable", "Top-ups are temporarily unavailable", "", 60)
}

func (h *handlers) getPayment(w http.ResponseWriter, r *http.Request) {
	if h.Payments == nil {
		paymentsUnavailable(w)
		return
	}
	p, _ := principalFrom(r.Context())
	t, err := h.Payments.Get(r.Context(), p.UserID, chi.URLParam(r, "id"))
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, toTopUpJSON(t))
	case errors.Is(err, payments.ErrNotFound):
		httpx.WriteProblem(w, http.StatusNotFound, "not-found", "Not found", "")
	default:
		internalError(w, err)
	}
}

// stkCallback receives Daraja's STK results. The last path segment is a secret only Daraja knows (it is in the
// callback URL we send with each push); anything else is a 404. The callback is stored and verified in the
// background; Daraja gets its acknowledgement at once.
func (h *handlers) stkCallback(w http.ResponseWriter, r *http.Request) {
	path := chi.URLParam(r, "path")
	if h.Payments == nil || h.CallbackPath == "" || subtle.ConstantTimeCompare([]byte(path), []byte(h.CallbackPath)) != 1 {
		httpx.WriteProblem(w, http.StatusNotFound, "not-found", "Not found", "")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "bad-callback", "Unreadable callback", "")
		return
	}
	switch err := h.Payments.HandleCallback(r.Context(), raw); {
	case errors.Is(err, daraja.ErrBadCallback):
		slog.Warn("malformed stk callback", "err", err)
		httpx.WriteProblem(w, http.StatusBadRequest, "bad-callback", "Malformed callback", "")
	case err != nil:
		internalError(w, err)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ResultCode": 0, "ResultDesc": "Accepted"})
	}
}
