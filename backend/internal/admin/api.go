package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"labyrinth/internal/platform/httpx"
	"labyrinth/internal/platform/identity"
	"labyrinth/internal/qte"
	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
	"labyrinth/internal/user"
)

// teamIDPattern matches a team code: exactly 6 digits (teams' format).
var teamIDPattern = regexp.MustCompile(`^[0-9]{6}$`)

// waypointIDPattern matches a waypoint ID: exactly 4 digits (the
// waypoint.id CHECK in migration 0003).
var waypointIDPattern = regexp.MustCompile(`^[0-9]{4}$`)

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
	mux.Handle("POST /api/admin/teams/{team_id}/powerups/{kind}", admin(a.grantPowerup))
	mux.Handle("GET /api/admin/qtes", admin(a.qtes))
	mux.Handle("POST /api/admin/qtes", admin(a.createQTE))
	mux.Handle("POST /api/admin/qtes/{qte_id}/start", admin(a.startQTE))
	mux.Handle("POST /api/admin/qtes/{qte_id}/end", admin(a.endQTE))
	mux.Handle("GET /api/admin/users", admin(a.searchUsers))
	mux.Handle("PUT /api/admin/users/{user_id}/role", admin(a.setRole))
	mux.Handle("GET /api/admin/helpers", admin(a.helpers))
	mux.Handle("POST /api/admin/helpers", admin(a.addHelper))
	mux.Handle("PUT /api/admin/helpers/{user_id}/waypoint", admin(a.assignWaypoint))
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
	{teamstate.ErrUnknownPowerup, http.StatusNotFound, "unknown_powerup"},
	{qte.ErrUnknownKind, http.StatusBadRequest, "invalid_kind"},
	{qte.ErrUnknownPowerup, http.StatusBadRequest, "invalid_powerup"},
	{qte.ErrInvalidQuestion, http.StatusBadRequest, "invalid_question"},
	{qte.ErrInvalidOptions, http.StatusBadRequest, "invalid_options"},
	{qte.ErrInvalidCorrectOption, http.StatusBadRequest, "invalid_correct_option"},
	{qte.ErrInvalidWindow, http.StatusBadRequest, "invalid_window"},
	{qte.ErrQTEOpen, http.StatusConflict, "qte_open"},
	{qte.ErrUnknownQTE, http.StatusNotFound, "unknown_qte"},
	{qte.ErrQTEAlreadyRun, http.StatusConflict, "qte_already_run"},
	{qte.ErrQTENotStarted, http.StatusConflict, "qte_not_started"},
	{qte.ErrQTEClosed, http.StatusConflict, "qte_closed"},
	{user.ErrUserNotFound, http.StatusNotFound, "user_not_found"},
	{user.ErrInvalidRole, http.StatusBadRequest, "invalid_role"},
	{ErrCannotChangeOwnRole, http.StatusConflict, "cannot_change_own_role"},
	{ErrAlreadyAdmin, http.StatusConflict, "already_admin"},
	{ErrUnknownWaypoint, http.StatusNotFound, "unknown_waypoint"},
	{user.ErrNotHelper, http.StatusConflict, "not_helper"},
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

// qteID returns the {qte_id} path value, or writes 400 invalid_qte_id and
// returns false.
func qteID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("qte_id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_qte_id", "qte id must be a positive integer")
		return 0, false
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

// grantPowerup handles POST /api/admin/teams/{team_id}/powerups/{kind}: the
// team gets a powerup of that kind, or keeps the one it holds (granted
// false). Returns {granted, state}.
func (a *API) grantPowerup(w http.ResponseWriter, r *http.Request) {
	id, ok := teamID(w, r)
	if !ok {
		return
	}
	kind, ok := teamstate.PowerupByID(r.PathValue("kind"))
	if !ok {
		a.fail(w, teamstate.ErrUnknownPowerup)
		return
	}
	res, err := a.admin.GrantPowerup(r.Context(), id, kind)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.log.Info("admin granted powerup", "admin_id", identity.UserID(r), "team_id", id, "kind", kind.ID(), "granted", res.Granted)
	httpx.OK(w, res)
}

// qtes handles GET /api/admin/qtes: every QTE, newest first, with its
// answer, status and the teams' answers.
func (a *API) qtes(w http.ResponseWriter, _ *http.Request) {
	httpx.OK(w, a.admin.QTEs())
}

// createQTE handles POST /api/admin/qtes {"kind", "powerup", "quiz":
// {"question", "options", "correct_option"}, "window_s"?, "start"}: a draft
// to launch later, or, with start, sent out at once. Returns its row.
func (a *API) createQTE(w http.ResponseWriter, r *http.Request) {
	// Its own shape, not qte.Draft's: correct_option is required, and only a
	// pointer tells a missing one from option 0.
	var req struct {
		Kind    string `json:"kind"`
		Powerup string `json:"powerup"`
		Quiz    *struct {
			Question      string   `json:"question"`
			Options       []string `json:"options"`
			CorrectOption *int     `json:"correct_option"`
		} `json:"quiz"`
		WindowS *int `json:"window_s"`
		Start   bool `json:"start"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil || (req.Quiz != nil && req.Quiz.CorrectOption == nil) {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_body",
			`body must be {"kind", "powerup", "quiz": {"question", "options", "correct_option"}, "window_s"?, "start"}`)
		return
	}
	d := qte.Draft{Kind: req.Kind, Powerup: req.Powerup, WindowS: req.WindowS}
	if q := req.Quiz; q != nil {
		d.Quiz = &qte.Quiz{Question: q.Question, Options: q.Options, CorrectOption: *q.CorrectOption}
	}
	adminID := identity.UserID(r)
	o, err := a.admin.CreateQTE(r.Context(), adminID, d, req.Start)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.log.Info("admin created qte", "admin_id", adminID, "qte_id", o.ID, "powerup", o.Powerup, "started", req.Start)
	httpx.OK(w, o)
}

// startQTE handles POST /api/admin/qtes/{qte_id}/start: a draft sent out
// now, open for its window. Returns its row.
func (a *API) startQTE(w http.ResponseWriter, r *http.Request) {
	id, ok := qteID(w, r)
	if !ok {
		return
	}
	o, err := a.admin.StartQTE(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.log.Info("admin started qte", "admin_id", identity.UserID(r), "qte_id", id)
	httpx.OK(w, o)
}

// endQTE handles POST /api/admin/qtes/{qte_id}/end: an open QTE closed now,
// its answer revealed to everyone. Returns its row.
func (a *API) endQTE(w http.ResponseWriter, r *http.Request) {
	id, ok := qteID(w, r)
	if !ok {
		return
	}
	o, err := a.admin.EndQTE(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.log.Info("admin ended qte", "admin_id", identity.UserID(r), "qte_id", id)
	httpx.OK(w, o)
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

// helpers handles GET /api/admin/helpers: every helper, by email, each with
// waypoint_id, their assigned checkpoint or null.
func (a *API) helpers(w http.ResponseWriter, r *http.Request) {
	hs, err := a.admin.Helpers(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, hs)
}

// addHelper handles POST /api/admin/helpers {"email"}: the account with
// that email (they must have signed in once) becomes a HELPER. Returns the
// user; one already a helper is returned unchanged.
func (a *API) addHelper(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_body", `body must be {"email": string}`)
		return
	}
	email := strings.TrimSpace(req.Email)
	if email == "" {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_email", "email must not be empty")
		return
	}
	u, err := a.admin.AddHelper(r.Context(), identity.UserID(r), email)
	if errors.Is(err, user.ErrUserNotFound) {
		httpx.ErrorCode(w, http.StatusNotFound, "user_not_found", "nobody with that email has signed in; they must sign in once first")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	a.log.Info("admin added helper", "admin_id", identity.UserID(r), "user_id", u.ID)
	httpx.OK(w, u)
}

// assignWaypoint handles PUT /api/admin/helpers/{user_id}/waypoint
// {"waypoint_id"}: the checkpoint the helper's /qr display shows, or null to
// unassign. Returns the user.
func (a *API) assignWaypoint(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(w, r.PathValue("user_id"), "user id")
	if !ok {
		return
	}
	// Raw, so a missing field (a bad body) differs from an explicit null
	// (unassign): decoding into a pointer can't tell them apart.
	var req struct {
		WaypointID json.RawMessage `json:"waypoint_id"`
	}
	var waypoint *string
	err := httpx.DecodeJSON(w, r, &req)
	if err == nil && len(req.WaypointID) > 0 {
		err = json.Unmarshal(req.WaypointID, &waypoint)
	}
	if err != nil || len(req.WaypointID) == 0 {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_body", `body must be {"waypoint_id": string | null}`)
		return
	}
	if waypoint != nil && !waypointIDPattern.MatchString(*waypoint) {
		httpx.ErrorCode(w, http.StatusBadRequest, "invalid_waypoint_id", "waypoint id must be 4 digits")
		return
	}
	u, err := a.admin.AssignWaypoint(r.Context(), uid, waypoint)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.log.Info("admin assigned helper", "admin_id", identity.UserID(r), "user_id", uid, "waypoint_id", waypoint)
	httpx.OK(w, u)
}
