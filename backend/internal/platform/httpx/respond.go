// Package httpx writes and reads JSON in the response envelope every route
// uses: {"success", "code", "message", "payload"}.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Response is the envelope for every JSON response, same shape as 2025's
// ApiResponse.
type Response struct {
	Success bool `json:"success"`
	// Code is a stable machine-readable error identifier, e.g. "team_full".
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Payload any    `json:"payload,omitempty"`
}

// internalError is the body sent when a response can't be encoded.
var internalError = []byte(`{"success":false,"message":"internal error"}` + "\n")

// JSON writes body as a JSON response with the given status. Responses are
// never cached. body is encoded before anything is written, so an encoding
// failure becomes a logged 500, never a truncated 200.
func JSON(w http.ResponseWriter, status int, body any) {
	data, err := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		slog.Error("encode response", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(internalError)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

// OK writes a 200 success envelope carrying payload.
func OK(w http.ResponseWriter, payload any) {
	JSON(w, http.StatusOK, Response{Success: true, Payload: payload})
}

// Error writes a failure envelope with a human-readable message.
func Error(w http.ResponseWriter, status int, message string) {
	JSON(w, status, Response{Success: false, Message: message})
}

// ErrorCode writes a failure envelope with a stable machine-readable code, for
// errors the frontend handles specifically.
func ErrorCode(w http.ResponseWriter, status int, code, message string) {
	JSON(w, status, Response{Success: false, Code: code, Message: message})
}

// DecodeJSON reads a JSON body into dst, rejecting unknown fields. The body's
// size is capped for every route by the server (config.toml
// server.max_body_bytes).
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
