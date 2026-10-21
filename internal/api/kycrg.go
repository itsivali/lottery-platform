package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/itsivali/lottery-platform/internal/kyc"
	"github.com/itsivali/lottery-platform/internal/platform/httpx"
	"github.com/itsivali/lottery-platform/internal/rg"
)

func internalError(w http.ResponseWriter, err error) {
	slog.Error("request failed", "err", err)
	httpx.WriteProblem(w, http.StatusInternalServerError, "internal", "Internal error", "")
}

func (h *handlers) kycStatus(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	st, err := h.KYC.Status(r.Context(), p.UserID)
	if err != nil {
		internalError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": string(st)})
}

func (h *handlers) kycSubmit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		NationalID  string `json:"national_id"`
		FullName    string `json:"full_name"`
		DateOfBirth string `json:"date_of_birth"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	p, _ := principalFrom(r.Context())
	st, err := h.KYC.Submit(r.Context(), p.UserID, kyc.Submission{IDNumber: in.NationalID, FullName: in.FullName, DateOfBirth: in.DateOfBirth})
	switch {
	case err == nil:
		// verification runs as a background job; the app polls GET /v1/kyc
		httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"status": string(st)})
	case errors.Is(err, kyc.ErrInvalidInput):
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid-kyc-input", "Invalid identity details", "check the ID number, name and date of birth")
	case errors.Is(err, kyc.ErrUnderage):
		httpx.WriteProblem(w, http.StatusForbidden, "underage", "You must be 18 or older to play", "")
	case errors.Is(err, kyc.ErrIDAlreadyRegistered):
		httpx.WriteProblem(w, http.StatusConflict, "id-already-registered", "This ID cannot be used", "contact support if you believe this is a mistake")
	case errors.Is(err, kyc.ErrAlreadyVerified), errors.Is(err, kyc.ErrInReview):
		httpx.WriteProblem(w, http.StatusConflict, "kyc-state", "Verification already submitted", err.Error())
	default:
		internalError(w, err)
	}
}

type limitJSON struct {
	AmountKES     int64     `json:"amount_kes"`
	EffectiveFrom time.Time `json:"effective_from"`
}

func toLimitJSON(l *rg.Limit) *limitJSON {
	if l == nil {
		return nil
	}
	return &limitJSON{AmountKES: l.AmountKES, EffectiveFrom: l.EffectiveFrom}
}

func (h *handlers) rgLimits(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	m, err := h.RG.Limits(r.Context(), p.UserID)
	if err != nil {
		internalError(w, err)
		return
	}
	out := map[string]map[string]*limitJSON{}
	for period, st := range m {
		out[string(period)] = map[string]*limitJSON{"current": toLimitJSON(st.Current), "pending": toLimitJSON(st.Pending)}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"limits": out})
}

func (h *handlers) rgSetLimit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Period    string `json:"period"`
		AmountKES int64  `json:"amount_kes"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	p, _ := principalFrom(r.Context())
	l, err := h.RG.SetLimit(r.Context(), p.UserID, rg.Period(in.Period), in.AmountKES)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, toLimitJSON(&l))
	case errors.Is(err, rg.ErrInvalidLimit):
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid-limit", "Invalid limit", "")
	default:
		internalError(w, err)
	}
}

func (h *handlers) rgTimeout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Duration string `json:"duration"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	p, _ := principalFrom(r.Context())
	h.restrictResult(w, h.RG.Timeout(r.Context(), p.UserID, in.Duration))
}

func (h *handlers) rgSelfExclude(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Term string `json:"term"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	p, _ := principalFrom(r.Context())
	h.restrictResult(w, h.RG.SelfExclude(r.Context(), p.UserID, in.Term))
}

func (h *handlers) restrictResult(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, rg.ErrInvalidDuration):
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid-duration", "Invalid duration", "")
	default:
		internalError(w, err)
	}
}
