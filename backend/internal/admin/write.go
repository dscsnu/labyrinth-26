package admin

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
	"labyrinth/internal/user"
)

// Every write is the owning use case's own write, run for an admin: its
// rules, its write-through, its events. admin adds only the checks that
// belong to who is acting (ErrCannotChangeOwnRole).

// ErrCannotChangeOwnRole means an admin tried to change their own role: an
// admin can't demote themselves (which could leave nobody able to undo it).
// Another admin can.
var ErrCannotChangeOwnRole = errors.New("you can't change your own role; ask another admin")

// RemoveMember removes userID from teamID (teams.RemoveMember): any status;
// newLeader names the next leader when userID leads it.
func (a *Admin) RemoveMember(ctx context.Context, teamID string, userID uuid.UUID, newLeader *uuid.UUID) (teams.Team, error) {
	return a.teams.RemoveMember(ctx, teamID, userID, newLeader)
}

// DeleteTeam deletes teamID and all its progress (teams.Delete): its members
// get no_team and team state forgets it.
func (a *Admin) DeleteTeam(ctx context.Context, teamID string) error {
	return a.teams.Delete(ctx, teamID)
}

// Revive restores teamID's health and restarts its boss's attack clock
// (teamstate.Revive).
func (a *Admin) Revive(ctx context.Context, teamID string) (teamstate.View, error) {
	return a.state.Revive(ctx, teamID)
}

// ResetCooldowns clears teamID's spell class and QR cooldowns
// (teamstate.ResetCooldowns).
func (a *Admin) ResetCooldowns(ctx context.Context, teamID string) (teamstate.View, error) {
	return a.state.ResetCooldowns(ctx, teamID)
}

// GrantPowerup gives teamID a powerup of kind (teamstate.GrantPowerup):
// Granted is false when the team already held one, so it's wasted.
func (a *Admin) GrantPowerup(ctx context.Context, teamID string, kind teamstate.Powerup) (teamstate.GrantResult, error) {
	return a.state.GrantPowerup(ctx, teamID, kind)
}

// SetRole gives userID role (user.SetRole), acting as adminID.
// ErrCannotChangeOwnRole if adminID is userID.
func (a *Admin) SetRole(ctx context.Context, adminID, userID uuid.UUID, role user.Role) (user.User, error) {
	if adminID == userID {
		return user.User{}, ErrCannotChangeOwnRole
	}
	return a.users.SetRole(ctx, userID, role)
}
