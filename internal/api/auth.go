package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/platform/httpx"
	"github.com/itsivali/lottery-platform/internal/platform/password"
)

type deviceJSON struct {
	Fingerprint      string `json:"fingerprint"`
	Platform         string `json:"platform"`
	AttestationToken string `json:"attestation_token"`
}

// toDevice converts the client's device description. The attestation token is only evidence for the server-side
// verifier; there is no way for a client to state a verdict.
func (d deviceJSON) toDevice() identity.Device {
	return identity.Device{Fingerprint: d.Fingerprint, Platform: d.Platform, AttestationToken: d.AttestationToken}
}

type tokensJSON struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func writeTokens(w http.ResponseWriter, t identity.Tokens) {
	httpx.WriteJSON(w, http.StatusOK, tokensJSON{AccessToken: t.Access, RefreshToken: t.Refresh, ExpiresAt: t.AccessExpires})
}

// authError maps identity errors to problem responses. Credential failures are deliberately uniform.
func authError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, identity.ErrInvalidMSISDN):
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid-msisdn", "Invalid phone number", "use a Kenyan mobile number such as 0712345678")
	case errors.Is(err, password.ErrWeak):
		httpx.WriteProblem(w, http.StatusBadRequest, "weak-password", "Password too weak", strings.TrimPrefix(err.Error(), "password: too weak: "))
	case errors.Is(err, identity.ErrInvalidCredentials):
		httpx.WriteProblem(w, http.StatusUnauthorized, "invalid-credentials", "Invalid phone number or password", "")
	case errors.Is(err, identity.ErrPhoneRegistered):
		httpx.WriteProblem(w, http.StatusConflict, "phone-registered", "This number already has an account", "sign in instead")
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
	case errors.Is(err, identity.ErrPhoneNotVerified):
		httpx.WriteProblem(w, http.StatusForbidden, "phone-not-verified", "Phone number not verified yet", "make your first top-up to verify it")
	default:
		internalError(w, err)
	}
}

type credentialsJSON struct {
	MSISDN   string     `json:"msisdn"`
	Password string     `json:"password"`
	Device   deviceJSON `json:"device"`
}

func (h *handlers) signup(w http.ResponseWriter, r *http.Request) {
	var in credentialsJSON
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	// limits first, keyed on the normalised number so formatting tricks cannot dodge them
	key := in.MSISDN
	if n, err := identity.NormalizeMSISDN(in.MSISDN); err == nil {
		key = n
	}
	if !h.limit(w, r, "signup:ip:"+h.clientIP(r), 10, time.Hour) || !h.limit(w, r, "signup:msisdn:"+key, 5, 24*time.Hour) {
		return
	}
	t, err := h.Identity.Signup(r.Context(), in.MSISDN, in.Password, in.Device.toDevice())
	if err != nil {
		authError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, tokensJSON{AccessToken: t.Access, RefreshToken: t.Refresh, ExpiresAt: t.AccessExpires})
}

func (h *handlers) login(w http.ResponseWriter, r *http.Request) {
	var in credentialsJSON
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	key := in.MSISDN
	if n, err := identity.NormalizeMSISDN(in.MSISDN); err == nil {
		key = n
	}
	// the per-number budget is what stops password guessing against one account, the per-IP one stops sweeping many
	if !h.limit(w, r, "login:msisdn:"+key, 10, time.Hour) || !h.limit(w, r, "login:ip:"+h.clientIP(r), 60, time.Hour) {
		return
	}
	t, err := h.Identity.Login(r.Context(), in.MSISDN, in.Password, in.Device.toDevice())
	if err != nil {
		authError(w, err)
		return
	}
	writeTokens(w, t)
}

func (h *handlers) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	p, _ := principalFrom(r.Context())
	if !h.limit(w, r, "pwchange:user:"+p.UserID, 10, time.Hour) {
		return
	}
	if err := h.Identity.ChangePassword(r.Context(), p.UserID, p.SessionID, in.Current, in.New); err != nil {
		if errors.Is(err, identity.ErrInvalidCredentials) {
			// 403, not 401: the caller's token is fine, so clients must not treat this as an expired session
			httpx.WriteProblem(w, http.StatusForbidden, "wrong-password", "Current password is incorrect", "")
			return
		}
		authError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
