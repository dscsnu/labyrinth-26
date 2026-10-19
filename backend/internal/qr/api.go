package qr

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"

	"labyrinth/internal/platform/httpx"
)

// waypointIDPattern matches a waypoint ID: exactly 4 digits. A format, not a
// tunable; it matches the waypoint.id CHECK in migration 0003.
var waypointIDPattern = regexp.MustCompile(`^[0-9]{4}$`)

// API serves the QR routes: minting a display token (staff) and verifying one
// (any signed-in user).
type API struct {
	verifier *Verifier
	log      *slog.Logger
}

// NewAPI returns the QR API backed by verifier.
func NewAPI(verifier *Verifier, log *slog.Logger) *API {
	return &API{verifier: verifier, log: log}
}

// Routes registers the QR routes any signed-in user may use; authed wraps a
// handler in the auth middleware.
func (a *API) Routes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST /api/qr/verify", authed(a.verify))
}

// StaffRoutes registers the QR routes only a HELPER or ADMIN may use; staff
// wraps a handler in the auth middleware plus the staff-role check.
func (a *API) StaffRoutes(mux *http.ServeMux, staff func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/qr/{waypoint_id}", staff(a.issue))
}

// errorCodes maps rule violations to an HTTP status and a stable code the
// frontend can switch on.
var errorCodes = []struct {
	err    error
	status int
	code   string
}{
	{ErrUnknownWaypoint, http.StatusNotFound, "unknown_waypoint"},
	{ErrInvalid, http.StatusBadRequest, "invalid_qr"},
	{ErrExpired, http.StatusConflict, "qr_expired"},
}

// fail writes the response for err: its status and code from errorCodes, or
// a 500 for anything unexpected (which is logged).
func (a *API) fail(w http.ResponseWriter, err error) {
	for _, e := range errorCodes {
		if errors.Is(err, e.err) {
			httpx.ErrorCode(w, e.status, e.code, e.err.Error())
			return
		}
	}
	a.log.Error("qr operation", "error", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}

// issue handles GET /api/qr/{waypoint_id}: the waypoint's signing key and
// timing rules, which the display uses to sign its own rotating tokens.
// Staff only (the caller's role was checked by the staff middleware).
func (a *API) issue(w http.ResponseWriter, r *http.Request) {
	waypointID := r.PathValue("waypoint_id")
	if !waypointIDPattern.MatchString(waypointID) {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_waypoint_id", "waypoint id must be 4 digits")
		return
	}
	key, err := a.verifier.SigningKey(waypointID)
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, key)
}

// verifyResult is the payload of a successful POST /api/qr/verify.
type verifyResult struct {
	// WaypointID is the 4-digit waypoint the token stands for.
	WaypointID string `json:"waypoint_id"`
}

// verify handles POST /api/qr/verify {"qr"}: the waypoint a token stands for,
// if it's signed and fresh. Any signed-in user may call it (it changes
// nothing); it exists so a scanner can be tested without touching team state.
func (a *API) verify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"qr"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil || req.Token == "" {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_body", `body must be {"qr": string}`)
		return
	}
	waypointID, err := a.verifier.Verify(req.Token)
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, verifyResult{WaypointID: waypointID})
}
