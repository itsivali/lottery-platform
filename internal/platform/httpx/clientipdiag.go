package httpx

import (
	"log/slog"
	"net/http"
	"strings"
)

// clientIPHeaders are the headers a proxy in front of us may use to carry the client address.
var clientIPHeaders = []string{"X-Forwarded-For", "X-Real-IP", "True-Client-IP", "CF-Connecting-IP", "Forwarded", "X-Client-IP", "Fly-Client-IP"}

// LogClientIPHeaders logs, for every request, the socket address and the address-carrying headers, so an operator can
// see what the hosting platform's load balancer actually sends (docs/runbooks/trusted-proxy.md). It logs nothing else:
// no query string, body, cookies or credentials. It is a diagnostic to switch on briefly (LOG_CLIENT_IP_HEADERS=true),
// never to leave on, since client addresses are personal data.
func LogClientIPHeaders(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attrs := []any{"path", r.URL.Path, "remote_addr", r.RemoteAddr}
		for _, h := range clientIPHeaders {
			if v := r.Header.Values(h); len(v) > 0 {
				attrs = append(attrs, strings.ToLower(h), strings.Join(v, " | "))
			}
		}
		log.Info("client ip headers", attrs...)
		next.ServeHTTP(w, r)
	})
}
