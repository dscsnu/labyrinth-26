package user

import (
	"context"
	"errors"
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
