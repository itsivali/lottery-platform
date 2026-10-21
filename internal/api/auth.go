package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/platform/httpx"
)

type deviceJSON struct {
	Fingerprint string `json:"fingerprint"`
	Platform    string `json:"platform"`
}

// toDevice converts the client's device description. The client's own attestation claim is never trusted: until the
// Play Integrity / App Attest verifier is wired in (phase 7) every device is recorded as unattested.
func (d deviceJSON) toDevice() identity.Device {
	return identity.Device{Fingerprint: d.Fingerprint, Platform: d.Platform, Attestation: "none"}
}

type tokensJSON struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func writeTokens(w http.ResponseWriter, t identity.Tokens) {
	httpx.WriteJSON(w, http.StatusOK, tokensJSON{AccessToken: t.Access, RefreshToken: t.Refresh, ExpiresAt: t.AccessExpires})
}

// authError maps identity errors to problem responses. OTP failures are deliberately uniform.
func authError(w http.ResponseWriter, err error) {
	var cd *identity.CooldownError
	switch {
	case errors.As(err, &cd):
		httpx.WriteProblemRetry(w, http.StatusTooManyRequests, "otp-cooldown", "Please wait before requesting another code", "", int(cd.RetryAfter.Seconds())+1)
	case errors.Is(err, identity.ErrInvalidMSISDN):
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid-msisdn", "Invalid phone number", "use a Kenyan mobile number such as 0712345678")
	case errors.Is(err, identity.ErrOTPInvalid):
		httpx.WriteProblem(w, http.StatusUnauthorized, "invalid-code", "Invalid or expired code", "")
	case errors.Is(err, identity.ErrTokenReuse), errors.Is(err, identity.ErrTokenInvalid):
		httpx.WriteProblem(w, http.StatusUnauthorized, "invalid-token", "Invalid token", "")
	case errors.Is(err, identity.ErrDeviceRequired):
		httpx.WriteProblem(w, http.StatusBadRequest, "device-required", "Device information required", "")
	case errors.Is(err, identity.ErrDeviceCap):
		httpx.WriteProblem(w, http.StatusForbidden, "device-limit", "Too many accounts on this device", "")
	case errors.Is(err, identity.ErrAccountSuspended):
		httpx.WriteProblem(w, http.StatusForbidden, "account-suspended", "Account suspended", "")
	case errors.Is(err, identity.ErrSSONotLinked):
		httpx.WriteProblem(w, http.StatusConflict, "sso-not-linked", "No account is linked to this identity",
			"sign in with your phone, complete verification, then link it")
	case errors.Is(err, identity.ErrSSOAlreadyLinked):
		httpx.WriteProblem(w, http.StatusConflict, "sso-already-linked", "Identity already linked", "")
	case errors.Is(err, identity.ErrKYCRequired):
		httpx.WriteProblem(w, http.StatusForbidden, "kyc-required", "Identity verification required", "")
	default:
		internalError(w, err)
	}
}

func (h *handlers) otpRequest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		MSISDN string `json:"msisdn"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	msisdn, err := identity.NormalizeMSISDN(in.MSISDN)
	if err != nil {
		authError(w, err)
		return
	}
	if !h.limit(w, r, "otp:msisdn:"+msisdn, 5, time.Hour) || !h.limit(w, r, "otp:ip:"+h.clientIP(r), 20, time.Hour) {
		return
	}
	if err := h.Identity.RequestOTP(r.Context(), msisdn, identity.PurposeLogin, h.clientIP(r)); err != nil {
		authError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) otpVerify(w http.ResponseWriter, r *http.Request) {
	var in struct {
		MSISDN string     `json:"msisdn"`
		Code   string     `json:"code"`
		Device deviceJSON `json:"device"`
	}
	if !httpx.DecodeJSON(w, r, &in) || !h.limit(w, r, "otp-verify:ip:"+h.clientIP(r), 30, time.Hour) {
		return
	}
	t, err := h.Identity.VerifyOTP(r.Context(), in.MSISDN, in.Code, in.Device.toDevice())
	if err != nil {
		authError(w, err)
		return
	}
	writeTokens(w, t)
}

func (h *handlers) refresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken      string `json:"refresh_token"`
		DeviceFingerprint string `json:"device_fingerprint"`
	}
	if !httpx.DecodeJSON(w, r, &in) || !h.limit(w, r, "refresh:ip:"+h.clientIP(r), 60, time.Hour) {
		return
	}
	t, err := h.Identity.Refresh(r.Context(), in.RefreshToken, in.DeviceFingerprint)
	if err != nil {
		authError(w, err)
		return
	}
	writeTokens(w, t)
}

func validProvider(p string) bool { return p == "google" || p == "apple" }

func (h *handlers) ssoSignIn(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if !validProvider(provider) {
		httpx.WriteProblem(w, http.StatusNotFound, "unknown-provider", "Unknown identity provider", "")
		return
	}
	var in struct {
		IDToken string     `json:"id_token"`
		Nonce   string     `json:"nonce"`
		Device  deviceJSON `json:"device"`
	}
	if !httpx.DecodeJSON(w, r, &in) || !h.limit(w, r, "sso:ip:"+h.clientIP(r), 30, time.Hour) {
		return
	}
	t, err := h.Identity.SignInSSO(r.Context(), provider, in.IDToken, in.Nonce, in.Device.toDevice())
	if err != nil {
		authError(w, err)
		return
	}
	writeTokens(w, t)
}

func (h *handlers) ssoLink(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if !validProvider(provider) {
		httpx.WriteProblem(w, http.StatusNotFound, "unknown-provider", "Unknown identity provider", "")
		return
	}
	var in struct {
		IDToken string `json:"id_token"`
		Nonce   string `json:"nonce"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	p, _ := principalFrom(r.Context())
	if err := h.Identity.LinkSSO(r.Context(), p.UserID, provider, in.IDToken, in.Nonce); err != nil {
		authError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) logout(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := h.Identity.Logout(r.Context(), p.SessionID); err != nil {
		internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
