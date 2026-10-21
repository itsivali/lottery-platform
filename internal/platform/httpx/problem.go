package httpx

import (
	"encoding/json"
	"net/http"
	"strconv"
)

const problemTypeBase = "https://lottery.example/problems/"

type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// WriteProblem writes an RFC 7807 problem+json response. slug identifies the problem type.
func WriteProblem(w http.ResponseWriter, status int, slug, title, detail string) {
	WriteProblemRetry(w, status, slug, title, detail, 0)
}

// WriteProblemRetry is WriteProblem plus a Retry-After header (seconds) when retryAfter > 0.
func WriteProblemRetry(w http.ResponseWriter, status int, slug, title, detail string, retryAfter int) {
	h := w.Header()
	h.Set("Content-Type", "application/problem+json")
	if retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(retryAfter))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{Type: problemTypeBase + slug, Title: title, Status: status, Detail: detail})
}
