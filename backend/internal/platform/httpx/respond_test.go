package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestJSONEncodeFailureIs500 checks that a body that can't be encoded gives a
// complete 500 envelope, not a truncated success.
func TestJSONEncodeFailureIs500(t *testing.T) {
	w := httptest.NewRecorder()
	OK(w, map[string]any{"bad": make(chan int)})
	if w.Code != http.StatusInternalServerError || w.Body.String() != string(internalError) {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}

// TestJSONWritesEnvelope checks a normal response's status, headers and body.
func TestJSONWritesEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	ErrorCode(w, http.StatusConflict, "team_full", "team is full")
	want := `{"success":false,"code":"team_full","message":"team is full"}` + "\n"
	if w.Code != http.StatusConflict || w.Body.String() != want ||
		w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("got %d %q %v", w.Code, w.Body.String(), w.Header())
	}
}
