package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/platform/postgres"
)

// Role is what a user may do: play, help on the ground, or administer.
type Role string

const (
	// RolePlayer is every new user's role.
	RolePlayer Role = "PLAYER"
	// RoleHelper is an on-ground volunteer.
	RoleHelper Role = "HELPER"
	// RoleAdmin runs the event.
	RoleAdmin Role = "ADMIN"
)

// User is one account, keyed by its Google subject ID.
type User struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	AvatarURL *string   `json:"avatar_url"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// Users is the user use case. Users aren't held in memory (sign-ins are rare),
// so reads and writes go straight to Postgres.
type Users struct {
	db db
}

// New returns the user use case backed by pg.
func New(pg *postgres.DB) *Users {
	return &Users{db: db{pool: pg.Pool()}}
}

// ErrUserNotFound means no user has that ID (e.g. deleted by hand after
// signing in).
var ErrUserNotFound = errors.New("user no longer exists")

// Get reads a user by ID. Returns ErrUserNotFound, wrapping
// postgres.ErrNotFound, if there's none.
func (u *Users) Get(ctx context.Context, id uuid.UUID) (User, error) {
	usr, err := u.db.selectByID(ctx, id)
	if errors.Is(err, postgres.ErrNotFound) {
		return usr, fmt.Errorf("%w: %w", ErrUserNotFound, err)
	}
	return usr, err
}
