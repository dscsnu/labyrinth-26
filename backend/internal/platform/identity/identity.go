// Package identity carries the signed-in user through a request. The auth
// middleware sets it; every other use case reads it, without depending on
// auth.
package identity

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// Identity is who made the request, from their verified token. Role is as of
// sign-in.
type Identity struct {
	UserID uuid.UUID
	Email  string
	Name   string
	Role   string
}

// key is the context key for the request's Identity.
type key struct{}

// With returns a copy of ctx carrying id.
func With(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, key{}, id)
}

// From returns the Identity on ctx, if the auth middleware set one.
func From(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(key{}).(Identity)
	return id, ok
}

// UserID is the signed-in user's ID. Only valid behind the auth middleware,
// which guarantees an identity.
func UserID(r *http.Request) uuid.UUID {
	id, _ := From(r.Context())
	return id.UserID
}
