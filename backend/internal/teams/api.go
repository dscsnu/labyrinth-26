package teams

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"labyrinth/internal/platform/httpx"
	"labyrinth/internal/platform/identity"
)

// teamIDPattern matches a team code: exactly 6 digits.
// It must match the team.id CHECK in migration 0002 and teamCodeMin/Max.
var teamIDPattern = regexp.MustCompile(`^[0-9]{6}$`)

// API serves the team routes.
type API struct {
	store *Store
	log   *slog.Logger
}

// NewAPI returns the team API backed by store.
func NewAPI(store *Store, log *slog.Logger) *API {
	return &API{store: store, log: log}
}

// Routes registers the team routes; authed wraps a handler in the auth
// middleware. The live stream (GET /api/teams/me/events) is registered by the
// server, since it carries events from several use cases.
func (a *API) Routes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/teams/me", authed(a.getMine))
	mux.Handle("POST /api/teams", authed(a.create))
	mux.Handle("POST /api/teams/join", authed(a.join))
	mux.Handle("POST /api/teams/leave", authed(a.leave))
	mux.Handle("PUT /api/teams/me/ready", authed(a.setReady))
	mux.Handle("POST /api/teams/me/start", authed(a.start))
}

// errorCodes maps rule violations to an HTTP status and a stable code the
// frontend can switch on.
var errorCodes = []struct {
	err    error
	status int
	code   string
}{
	{ErrNotInTeam, http.StatusNotFound, "not_in_team"},
	{ErrTeamNotFound, http.StatusNotFound, "team_not_found"},
	{ErrAlreadyInTeam, http.StatusConflict, "already_in_team"},
	{ErrTeamFull, http.StatusConflict, "team_full"},
	{ErrTeamLocked, http.StatusConflict, "team_locked"},
	{ErrNotLeader, http.StatusForbidden, "not_leader"},
	{ErrLeaderReady, http.StatusConflict, "leader_cannot_ready"},
	{ErrTeamNotFull, http.StatusConflict, "team_not_full"},
	{ErrNotAllReady, http.StatusConflict, "not_all_ready"},
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
	a.log.Error("team operation", "error", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}

// getMine handles GET /api/teams/me: the caller's team, or 404 not_in_team.
func (a *API) getMine(w http.ResponseWriter, r *http.Request) {
	t, err := a.store.ForUser(identity.UserID(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, t)
}

// create handles POST /api/teams {"name"}: a new team with the caller as
// leader, 201 on success. The name is trimmed here, then must be 1 to
// teams.name_max_length characters with no control characters (validName).
func (a *API) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_body", `body must be {"name": string}`)
		return
	}
	name := strings.TrimSpace(req.Name)
	if !validName(name, a.store.rules.NameMaxLength) {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_team_name",
			fmt.Sprintf("team name must be 1-%d characters with no control characters", a.store.rules.NameMaxLength))
		return
	}
	t, err := a.store.Create(r.Context(), identity.UserID(r), name)
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, httpx.Response{Success: true, Payload: t})
}

// join handles POST /api/teams/join {"team_id"}: the caller joins that team.
func (a *API) join(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TeamID string `json:"team_id"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_body", `body must be {"team_id": string}`)
		return
	}
	if !teamIDPattern.MatchString(req.TeamID) {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_team_id", "team id must be 6 digits")
		return
	}
	t, err := a.store.Join(r.Context(), identity.UserID(r), req.TeamID)
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, t)
}

// leave handles POST /api/teams/leave: the caller leaves their team.
func (a *API) leave(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Leave(r.Context(), identity.UserID(r)); err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, nil)
}

// setReady handles PUT /api/teams/me/ready {"ready"}: sets the caller's ready
// flag. "ready" is required.
func (a *API) setReady(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ready *bool `json:"ready"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil || req.Ready == nil {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_body", `body must be {"ready": boolean}`)
		return
	}
	t, err := a.store.SetReady(r.Context(), identity.UserID(r), *req.Ready)
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, t)
}

// start handles POST /api/teams/me/start: the leader starts the game.
func (a *API) start(w http.ResponseWriter, r *http.Request) {
	t, err := a.store.Start(r.Context(), identity.UserID(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, t)
}

// validName reports whether name is 1 to maxLength (teams.name_max_length)
// characters with no control characters. It doesn't trim: create trims
// before calling it.
func validName(name string, maxLength int) bool {
	if name == "" || utf8.RuneCountInString(name) > maxLength {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
