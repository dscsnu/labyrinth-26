package admin

import (
	"context"

	"github.com/google/uuid"

	"labyrinth/internal/qte"
	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
	"labyrinth/internal/user"
)

// Teams is what admin needs from the teams use case.
type Teams interface {
	All() []teams.Team
	ByID(teamID string) (teams.Team, bool)
	RemoveMember(ctx context.Context, teamID string, userID uuid.UUID, newLeader *uuid.UUID) (teams.Team, error)
	Delete(ctx context.Context, teamID string) error
}

// TeamState is what admin needs from the teamstate use case.
type TeamState interface {
	TeamView(teamID string) teamstate.View
	Revive(ctx context.Context, teamID string) (teamstate.View, error)
	ResetCooldowns(ctx context.Context, teamID string) (teamstate.View, error)
	GrantPowerup(ctx context.Context, teamID string, kind teamstate.Powerup) (teamstate.GrantResult, error)
	QTEStats() map[int64]teamstate.QTEStat
}

// QTEs is what admin needs from the qte use case.
type QTEs interface {
	List() []qte.QTE
	Status(q qte.QTE) string
	Create(ctx context.Context, adminID uuid.UUID, d qte.Draft, start bool) (qte.QTE, error)
	Start(ctx context.Context, id int64) (qte.QTE, error)
	End(ctx context.Context, id int64) (qte.QTE, error)
}

// Users is what admin needs from the user use case.
type Users interface {
	Search(ctx context.Context, query string, limit int) ([]user.User, error)
	SetRole(ctx context.Context, id uuid.UUID, role user.Role) (user.User, error)
	ByEmail(ctx context.Context, email string) (user.User, error)
	Helpers(ctx context.Context) ([]user.User, error)
	Assign(ctx context.Context, id uuid.UUID, waypointID *string) (user.User, error)
}

// Atlas is what admin needs from the atlas use case: whether a waypoint
// exists, so a helper can't be assigned a made-up checkpoint. The atlas never
// changes after startup, so reading it needs no lock.
type Atlas interface {
	WaypointIndex(id string) (int, bool)
}

// Rules are admin's settings from config.toml.
type Rules struct {
	// UserSearchLimit is the most users a search returns
	// (admin.user_search_limit).
	UserSearchLimit int
}

// Admin is the admin use case: the other use cases it acts on, and its
// rules. Stateless.
type Admin struct {
	teams Teams
	state TeamState
	qtes  QTEs
	users Users
	atlas Atlas
	rules Rules
}

// New returns the admin use case over the given use cases.
func New(t Teams, st TeamState, q QTEs, u Users, at Atlas, rules Rules) *Admin {
	return &Admin{teams: t, state: st, qtes: q, users: u, atlas: at, rules: rules}
}

// TeamOverview is one team as the admin's teams list shows it: the team, and
// for a started or finished team its game at a glance.
type TeamOverview struct {
	// Team is the team: members, leader, status.
	Team teams.Team `json:"team"`
	// Game is its game, or null for a team still in the lobby.
	Game *GameOverview `json:"game"`
}

// GameOverview is what an admin needs to see about a team's game: whether
// it needs a revive, where it is, and what's cooling down.
type GameOverview struct {
	// Health is its health; KnockedOut is health 0 (needs a revive).
	Health     int  `json:"health"`
	KnockedOut bool `json:"knocked_out"`
	// BossesDefeated and Boss: how far it has got (Boss is null after the
	// last).
	BossesDefeated int                 `json:"bosses_defeated"`
	Boss           *teamstate.BossView `json:"boss"`
	// CooldownClass and QRCooldowns: what a cooldown reset would clear.
	CooldownClass *string                    `json:"cooldown_class"`
	QRCooldowns   []teamstate.QRCooldownView `json:"qr_cooldowns"`
	// CastCount is how many spells it has cast.
	CastCount int `json:"cast_count"`
	// Powerups are the powerups it holds and the ones running.
	Powerups teamstate.PowerupsView `json:"powerups"`
}

// Teams returns every team, by ID, each with its game at a glance. Read
// from memory.
func (a *Admin) Teams() []TeamOverview {
	all := a.teams.All()
	out := make([]TeamOverview, len(all))
	for i, t := range all {
		out[i] = TeamOverview{Team: t}
		if t.Status == teams.Lobby {
			continue
		}
		v := a.state.TeamView(t.ID)
		out[i].Game = &GameOverview{
			Health: v.Health, KnockedOut: v.KnockedOut, BossesDefeated: v.BossesDefeated, Boss: v.Boss,
			CooldownClass: v.CooldownClass, QRCooldowns: v.QRCooldowns, CastCount: v.CastCount, Powerups: v.Powerups,
		}
	}
	return out
}

// QTEOverview is one QTE as the admin's list shows it: all of it (its
// answer included), its status now, and how the teams answered.
type QTEOverview struct {
	// QTE is the QTE, flattened into the object.
	qte.QTE
	// Status is "draft", "open" or "closed", on the server's clock.
	Status string `json:"status"`
	// Stats counts the teams' answers.
	Stats teamstate.QTEStat `json:"stats"`
}

// QTEs returns every QTE, newest first, each with its status and answers.
// Read from memory.
func (a *Admin) QTEs() []QTEOverview {
	all := a.qtes.List()
	stats := a.state.QTEStats()
	out := make([]QTEOverview, len(all))
	for i, q := range all {
		out[i] = a.overview(q, stats)
	}
	return out
}

// overview is q's row, its answers counted from stats.
func (a *Admin) overview(q qte.QTE, stats map[int64]teamstate.QTEStat) QTEOverview {
	st := stats[q.ID]
	if st.ByOption == nil {
		st.ByOption = map[int]int{}
	}
	return QTEOverview{QTE: q, Status: a.qtes.Status(q), Stats: st}
}

// SearchUsers returns up to admin.user_search_limit users whose email
// contains query, case insensitive, by email.
func (a *Admin) SearchUsers(ctx context.Context, query string) ([]user.User, error) {
	return a.users.Search(ctx, query, a.rules.UserSearchLimit)
}

// Helpers returns every helper, by email, with their assigned checkpoints
// (user.Helpers).
func (a *Admin) Helpers(ctx context.Context) ([]user.User, error) {
	return a.users.Helpers(ctx)
}
