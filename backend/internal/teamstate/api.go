package teamstate

import (
	"errors"
	"log/slog"
	"net/http"

	"labyrinth/internal/platform/httpx"
	"labyrinth/internal/platform/identity"
)

// API serves the team state routes.
type API struct {
	store *Store
	log   *slog.Logger
}

// NewAPI returns the team state API backed by store.
func NewAPI(store *Store, log *slog.Logger) *API {
	return &API{store: store, log: log}
}

// Routes registers the team state routes; authed wraps a handler in the auth
// middleware.
func (a *API) Routes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/teams/me/state", authed(a.getMine))
	mux.Handle("GET /api/teams/me/casts", authed(a.casts))
	mux.Handle("PUT /api/teams/me/state/selection", authed(a.selectShape))
	mux.Handle("DELETE /api/teams/me/state/selection", authed(a.clearSelection))
	mux.Handle("POST /api/teams/me/state/scans", authed(a.scan))
	mux.Handle("GET /api/game/shapes", authed(a.shapes))
}

// AdminRoutes registers the admin team state routes; admin wraps a handler in
// the auth middleware plus the admin-role check.
func (a *API) AdminRoutes(mux *http.ServeMux, admin func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST /api/admin/teams/{team_id}/revive", admin(a.revive))
}

// revive handles POST /api/admin/teams/{team_id}/revive: an admin restores a
// started team's health to game.health and restarts its current boss's
// attack clock; the team's state after. 404 team_not_found for an unknown
// ID (any malformed one included), 409 not_in_game unless it's IN_GAME.
// Logged with the admin's ID.
func (a *API) revive(w http.ResponseWriter, r *http.Request) {
	teamID := r.PathValue("team_id")
	v, err := a.store.Revive(r.Context(), teamID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.log.Info("admin revived team", "admin_id", identity.UserID(r), "team_id", teamID)
	httpx.OK(w, v)
}

// errorCodes maps rule violations to an HTTP status and a stable code the
// frontend can switch on.
var errorCodes = []struct {
	err    error
	status int
	code   string
}{
	{ErrNotInTeam, http.StatusNotFound, "not_in_team"},
	{ErrUnknownWaypoint, http.StatusNotFound, "unknown_waypoint"},
	{ErrUnknownShape, http.StatusNotFound, "unknown_shape"},
	{ErrNotInGame, http.StatusConflict, "not_in_game"},
	{ErrNoShapeSelected, http.StatusConflict, "no_shape_selected"},
	{ErrWaypointAlreadyScanned, http.StatusConflict, "waypoint_already_scanned"},
	{ErrSelectionPoisoned, http.StatusConflict, "selection_poisoned"},
	{ErrKnockedOut, http.StatusConflict, "knocked_out"},
	{ErrNotLeader, http.StatusForbidden, "not_leader"},
	{ErrShapeLocked, http.StatusConflict, "shape_locked"},
	{ErrTeamNotFound, http.StatusNotFound, "team_not_found"},
	{ErrInvalidQR, http.StatusBadRequest, "invalid_qr"},
	{ErrExpiredQR, http.StatusConflict, "qr_expired"},
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
	a.log.Error("team state operation", "error", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}

// shapes handles GET /api/game/shapes: the shapes a team can select. Any
// signed-in user may read it.
func (a *API) shapes(w http.ResponseWriter, r *http.Request) {
	httpx.OK(w, a.store.Game())
}

// casts handles GET /api/teams/me/casts: every pattern the caller's team has
// completed, repeats included, newest first, or 404 not_in_team.
func (a *API) casts(w http.ResponseWriter, r *http.Request) {
	cs, err := a.store.CastsForUser(identity.UserID(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, cs)
}

// getMine handles GET /api/teams/me/state: the caller's team state, or 404
// not_in_team.
func (a *API) getMine(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.ForUser(identity.UserID(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, v)
}

// selectShape handles PUT /api/teams/me/state/selection {"shape"}: any member
// selects the shape the team will attempt. Returns the team state.
func (a *API) selectShape(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Shape string `json:"shape"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil || req.Shape == "" {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_body", `body must be {"shape": string}`)
		return
	}
	v, err := a.store.Select(r.Context(), identity.UserID(r), req.Shape)
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, v)
}

// clearSelection handles DELETE /api/teams/me/state/selection: removes the
// selected shape and purges the scan stack. Returns the team state.
func (a *API) clearSelection(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.ClearSelection(r.Context(), identity.UserID(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, v)
}

// scan handles POST /api/teams/me/state/scans {"qr"}: the caller scans a
// waypoint by submitting its signed, fresh QR token. Returns a ScanResult.
func (a *API) scan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"qr"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil || req.Token == "" {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_body", `body must be {"qr": string}`)
		return
	}
	res, err := a.store.Scan(r.Context(), identity.UserID(r), req.Token)
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, res)
}
