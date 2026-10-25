package user

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"labyrinth/internal/platform/postgres"
)

// ErrEmailInUse means a different Google account's user already has this
// email (e.g. an address moved between Google accounts).
var ErrEmailInUse = errors.New("email already belongs to another account")

// GoogleProfile is what Google's verified ID token says about a user.
type GoogleProfile struct {
	Subject   string
	Email     string
	Name      string
	AvatarURL string
}

// SignInWithGoogle creates the user on first sign-in, or refreshes their name,
// email and avatar on later ones (an empty AvatarURL keeps the stored one).
// Role is left as it is. ErrEmailInUse if another user has the email.
func (u *Users) SignInWithGoogle(ctx context.Context, p GoogleProfile) (User, error) {
	return u.db.upsertGoogle(ctx, p)
}

// ErrInvalidRole means the role isn't PLAYER, HELPER or ADMIN.
var ErrInvalidRole = errors.New("role must be PLAYER, HELPER or ADMIN")

// SetRole changes a user's role (an admin's action). It applies to every
// route that reads the role from here (RequireAdmin, RequireStaff) at once;
// the token's copy updates at the user's next sign-in. Demoting to PLAYER
// also clears their assigned checkpoint (only staff can hold one).
// ErrInvalidRole for an unknown role; ErrUserNotFound if there's no such
// user.
func (u *Users) SetRole(ctx context.Context, id uuid.UUID, role Role) (User, error) {
	switch role {
	case RolePlayer, RoleHelper, RoleAdmin:
	default:
		return User{}, ErrInvalidRole
	}
	usr, err := u.db.updateRole(ctx, id, role)
	if errors.Is(err, postgres.ErrNotFound) {
		return usr, fmt.Errorf("%w: %w", ErrUserNotFound, err)
	}
	return usr, err
}

// ErrNotHelper means the user isn't a HELPER, so they can't be assigned a
// checkpoint (an admin picks theirs on /qr; a player has no display).
var ErrNotHelper = errors.New("user is not a helper")

// Assign sets the checkpoint a helper's /qr display shows, or clears it
// when waypointID is nil (an admin's action). It doesn't check the waypoint
// exists: that's the caller's rule (admin checks the atlas); the foreign key
// is the backstop. ErrNotHelper if the user isn't a HELPER; ErrUserNotFound
// if there's no such user.
func (u *Users) Assign(ctx context.Context, id uuid.UUID, waypointID *string) (User, error) {
	usr, err := u.db.updateWaypoint(ctx, id, waypointID)
	if !errors.Is(err, postgres.ErrNotFound) {
		return usr, err
	}
	// No helper row matched: tell an unknown user from a non-helper.
	if _, err := u.Get(ctx, id); err != nil {
		return User{}, err
	}
	return User{}, ErrNotHelper
}
