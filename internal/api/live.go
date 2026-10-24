package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/itsivali/lottery-platform/internal/payments"
	"github.com/itsivali/lottery-platform/internal/platform/httpx"
)

// liveHeartbeat (a variable for tests) is how often an idle stream sends a comment (proxies close silent connections) and re-checks the
// session behind it, so a stream ends soon after its token expires or its session is revoked.
var liveHeartbeat = 25 * time.Second

// paymentEvents streams the player's payment changes as server-sent events. It starts with every payment still
// being settled or changed recently (a top-up that settled before the stream opened is not missed), then sends each
// change as it commits. A "resync" event asks nothing of the client: the stream sends the snapshot again itself.
// The stream ends when the access token expires or the session is revoked; the client reconnects with a fresh token.
func (h *handlers) paymentEvents(w http.ResponseWriter, r *http.Request) {
	if h.Payments == nil || h.Live == nil {
		paymentsUnavailable(w)
		return
	}
	p, _ := principalFrom(r.Context())
	if !h.limit(w, r, "events:user:"+p.UserID, 60, time.Hour) {
		return
	}
	sub, err := h.Live.Subscribe(p.UserID)
	switch {
	case errors.Is(err, payments.ErrTooManyStreams):
		httpx.WriteProblemRetry(w, http.StatusTooManyRequests, "too-many-streams", "Too many open streams", "close another stream first", 30)
		return
	case err != nil:
		paymentsUnavailable(w)
		return
	}
	defer sub.Close()

	rc := http.NewResponseController(w)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	ctx := r.Context()
	sent := map[string]topUpJSON{}

	write := func(ev string, v any) bool {
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	// payment sends a payment unless the client already has exactly this state of it.
	payment := func(t payments.TopUp) bool {
		j := toTopUpJSON(t)
		if prev, ok := sent[j.ID]; ok && prev.Status == j.Status && equalAmount(prev.AmountConfirmedKES, j.AmountConfirmedKES) {
			return true
		}
		sent[j.ID] = j
		return write("payment", j)
	}
	snapshot := func() bool {
		list, err := h.Payments.Recent(ctx, p.UserID)
		if err != nil {
			slog.WarnContext(ctx, "payment stream snapshot failed", "err", err)
			return false
		}
		for i := len(list) - 1; i >= 0; i-- { // oldest first, as they happened
			if !payment(list[i]) {
				return false
			}
		}
		return true
	}

	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil || !snapshot() || !write("ready", struct{}{}) {
		return
	}
	tick := time.NewTicker(liveHeartbeat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C:
			if !ok { // shutting down, or this stream fell behind: the client reconnects
				return
			}
			if ev.Resync {
				if !snapshot() {
					return
				}
				continue
			}
			t, err := h.Payments.Get(ctx, p.UserID, ev.PaymentID)
			if errors.Is(err, payments.ErrNotFound) {
				continue
			}
			if err != nil || !payment(t) {
				return
			}
		case <-tick.C:
			if _, err := h.Identity.Authenticate(ctx, token); err != nil {
				_ = write("expired", struct{}{})
				return
			}
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

func equalAmount(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
