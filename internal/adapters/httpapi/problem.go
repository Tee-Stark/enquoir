package httpapi

import (
	"encoding/json"
	"net/http"
)

// Problem codes: stable, machine-readable reasons a client can branch
// on. The human-readable detail may change; these may not.
const (
	codeInvalidJSON      = "invalid_json"
	codeValidationFailed = "validation_failed"
	codeNotFound         = "not_found"
	codeRailRejected     = "rail_rejected"
	codeInternal         = "internal_error"
)

// problem is an RFC 9457 problem details object, with a code extension.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
}

func writeProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
		Code:   code,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
