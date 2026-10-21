package admin

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/google/uuid"

	"labyrinth/internal/platform/httpx"
	"labyrinth/internal/platform/identity"
	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
	"labyrinth/internal/user"
)

// teamIDPattern matches a team code: exactly 6 digits (teams' format).
var teamIDPattern = regexp.MustCompile(`^[0-9]{6}$`)

// API serves the admin routes.
type API struct {
	admin *Admin
	log   *slog.Logger
}

// NewAPI returns the admin API backed by a.
func NewAPI(a *Admin, log *slog.Logger) *API { return &API{admin: a, log: log} }

// Routes registers every admin route; admin wraps a handler in the auth
// middleware plus the admin-role check (server.Build).
func (a *API) Routes(mux *http.ServeMux, admin func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/admin/teams", admin(a.teams))
	mux.Handle("DELETE /api/admin/teams/{team_id}", admin(a.deleteTeam))
	mux.Handle("DELETE /api/admin/teams/{team_id}/members/{user_id}", admin(a.removeMember))
	mux.Handle("POST /api/admin/teams/{team_id}/revive", admin(a.revive))
	mux.Handle("POST /api/admin/teams/{team_id}/cooldowns/reset", admin(a.resetCooldowns))
	mux.Handle("GET /api/admin/users", admin(a.searchUsers))
	mux.Handle("PUT /api/admin/users/{user_id}/role", admin(a.setRole))
}

// errorCodes maps the rule errors these routes can meet, from admin and the
// use cases it calls, to a status and a stable code.
var errorCodes = []struct {
	err    error
	status int
	code   string
}{
	{teams.ErrTeamNotFound, http.StatusNotFound, "team_not_found"},
	{teams.ErrNotMember, http.StatusNotFound, "not_a_member"},
	{teams.ErrLastMember, http.StatusConflict, "last_member"},
	{teams.ErrNewLeaderRequired, http.StatusConflict, "new_leader_required"},
	{teams.ErrInvalidNewLeader, http.StatusConflict, "invalid_new_leader"},
	{teamstate.ErrNotInGame, http.StatusConflict, "not_in_game"},
	{user.ErrUserNotFound, http.StatusNotFound, "user_not_found"},
	{user.ErrInvalidRole, http.StatusBadRequest, "invalid_role"},
	{ErrCannotChangeOwnRole, http.StatusConflict, "cannot_change_own_role"},
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
	a.log.Error("admin operation", "error", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}

// teamID returns the {team_id} path value, or writes 400 invalid_team_id
// and returns false.
func teamID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("team_id")
	if !teamIDPattern.MatchString(id) {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_team_id", "team id must be 6 digits")
		return "", false
	}
	return id, true
}

// userID parses a user ID from raw (a path or query value), or writes 400
// invalid_user_id naming what and returns false.
func userID(w http.ResponseWriter, raw, what string) (uuid.UUID, bool) {
	id, err := uuid.Parse(raw)
	if err != nil {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_user_id", what+" must be a UUID")
		return uuid.UUID{}, false
	}
	return id, true
}

// teams handles GET /api/admin/teams: every team, by ID, with its game at a
// glance (health, knocked out, boss, cooldowns) for a started one.
func (a *API) teams(w http.ResponseWriter, _ *http.Request) {
	httpx.OK(w, a.admin.Teams())
}

// deleteTeam handles DELETE /api/admin/teams/{team_id}: the team and all its
// progress are deleted; its members get no_team. 200 with no payload.
func (a *API) deleteTeam(w http.ResponseWriter, r *http.Request) {
	id, ok := teamID(w, r)
	if !ok {
		return
	}
	if err := a.admin.DeleteTeam(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	a.log.Info("admin deleted team", "admin_id", identity.UserID(r), "team_id", id)
	httpx.OK(w, nil)
}

// removeMember handles DELETE /api/admin/teams/{team_id}/members/{user_id}
// [?new_leader=<user id>]: removes a member, naming the new leader when the
// member leads the team. Returns the team after the removal.
func (a *API) removeMember(w http.ResponseWriter, r *http.Request) {
	tid, ok := teamID(w, r)
	if !ok {
		return
	}
	uid, ok := userID(w, r.PathValue("user_id"), "user id")
	if !ok {
		return
	}
	var newLeader *uuid.UUID
	if raw := r.URL.Query().Get("new_leader"); raw != "" {
		id, ok := userID(w, raw, "new_leader")
		if !ok {
			return
		}
		newLeader = &id
	}
	t, err := a.admin.RemoveMember(r.Context(), tid, uid, newLeader)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.log.Info("admin removed team member", "admin_id", identity.UserID(r), "team_id", tid, "user_id", uid, "new_leader", newLeader)
	httpx.OK(w, t)
}

// revive handles POST /api/admin/teams/{team_id}/revive: health back to
// game.health and the boss's attack clock restarted. The team's state after.
func (a *API) revive(w http.ResponseWriter, r *http.Request) {
	id, ok := teamID(w, r)
	if !ok {
		return
	}
	v, err := a.admin.Revive(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.log.Info("admin revived team", "admin_id", identity.UserID(r), "team_id", id)
	httpx.OK(w, v)
}

// resetCooldowns handles POST /api/admin/teams/{team_id}/cooldowns/reset:
// no spell class or waypoint is on cooldown for the team any more. The
// team's state after.
func (a *API) resetCooldowns(w http.ResponseWriter, r *http.Request) {
	id, ok := teamID(w, r)
	if !ok {
		return
	}
	v, err := a.admin.ResetCooldowns(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.log.Info("admin reset team cooldowns", "admin_id", identity.UserID(r), "team_id", id)
	httpx.OK(w, v)
}

// searchUsers handles GET /api/admin/users[?email=<part>]: up to
// admin.user_search_limit users whose email contains it, by email.
func (a *API) searchUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.admin.SearchUsers(r.Context(), r.URL.Query().Get("email"))
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, users)
}

// setRole handles PUT /api/admin/users/{user_id}/role {"role"}: gives the
// user PLAYER, HELPER or ADMIN. An admin can't change their own. Returns
// the user.
func (a *API) setRole(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(w, r.PathValue("user_id"), "user id")
	if !ok {
		return
	}
	var req struct {
		Role user.Role `json:"role"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil || req.Role == "" {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_body", `body must be {"role": "PLAYER" | "HELPER" | "ADMIN"}`)
		return
	}
	u, err := a.admin.SetRole(r.Context(), identity.UserID(r), uid, req.Role)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.log.Info("admin set role", "admin_id", identity.UserID(r), "user_id", uid, "role", req.Role)
	httpx.OK(w, u)
}
