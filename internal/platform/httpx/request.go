package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

const maxJSONBody = 1 << 20

// DecodeJSON strictly decodes exactly one JSON object into v: unknown fields, trailing data, wrong types and bodies
// over 1 MiB are rejected. On failure it writes a problem response and returns false.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err == nil && !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		err = errors.New("body must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err == nil {
		err = dec.Decode(v)
	}
	if err == nil {
		if _, perr := dec.Token(); !errors.Is(perr, io.EOF) {
			err = errors.New("unexpected data after JSON body")
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			WriteProblem(w, http.StatusRequestEntityTooLarge, "body-too-large", "Request body too large", "")
			return false
		}
		WriteProblem(w, http.StatusBadRequest, "invalid-request", "Invalid request body", err.Error())
		return false
	}
	return true
}

// ClientIP is ClientIPHops with a single trusted proxy.
func ClientIP(r *http.Request, trustedHeader string) string { return ClientIPHops(r, trustedHeader, 1) }

// ClientIPHops returns the caller's address. trustedHeader names the header set by our own proxy chain (for example
// "True-Client-IP", or "X-Forwarded-For"); it is empty when not behind a proxy, in which case headers are ignored
// entirely so a client cannot spoof its address. For X-Forwarded-For, hops is the number of trusted proxies that each
// appended one entry: the client is the hops-th entry from the right, so entries the client invented on the left are
// never used. If the chain is shorter than hops or the entry is not an address, the socket address is used.
func ClientIPHops(r *http.Request, trustedHeader string, hops int) string {
	if trustedHeader != "" {
		v := r.Header.Get(trustedHeader)
		if strings.EqualFold(trustedHeader, "X-Forwarded-For") {
			hops = max(hops, 1)
			parts := strings.Split(v, ",")
			if len(parts) >= hops {
				v = parts[len(parts)-hops]
			} else {
				v = ""
			}
		}
		if a, err := netip.ParseAddr(strings.TrimSpace(v)); err == nil {
			return a.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
