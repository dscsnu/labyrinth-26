package tests

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"labyrinth/internal/platform/httpx"
)

// TestJSONEncodeFailureIs500 checks that a body that can't be encoded gives a
// complete 500 envelope, not a truncated success.
func TestJSONEncodeFailureIs500(t *testing.T) {
	w := httptest.NewRecorder()
	httpx.OK(w, map[string]any{"bad": make(chan int)})
	if w.Code != http.StatusInternalServerError || w.Body.String() != `{"success":false,"message":"internal error"}`+"\n" {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}

// TestJSONWritesEnvelope checks a normal response's status, headers and body.
func TestJSONWritesEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	httpx.ErrorCode(w, http.StatusConflict, "team_full", "team is full")
	want := `{"success":false,"code":"team_full","message":"team is full"}` + "\n"
	if w.Code != http.StatusConflict || w.Body.String() != want ||
		w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("got %d %q %v", w.Code, w.Body.String(), w.Header())
	}
}
