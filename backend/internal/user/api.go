package user

import (
	"errors"
	"log/slog"
	"net/http"

	"labyrinth/internal/platform/httpx"
	"labyrinth/internal/platform/identity"
)

// API serves the user routes.
type API struct {
	users *Users
	log   *slog.Logger
}

// NewAPI returns the user API backed by users.
func NewAPI(users *Users, log *slog.Logger) *API {
	return &API{users: users, log: log}
}

// Routes registers the user routes; authed wraps a handler in the auth
// middleware.
func (a *API) Routes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/me", authed(a.me))
}

// errorCodes maps rule violations to an HTTP status and a stable code the
// frontend can switch on. ErrUserNotFound is 401, not 404: the caller's valid
// token names a user that no longer exists, which is an authentication
// failure (sign in again), not a missing resource.
var errorCodes = []struct {
	err    error
	status int
	code   string
}{
	{ErrUserNotFound, http.StatusUnauthorized, "user_not_found"},
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
	a.log.Error("load user", "error", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}

// me handles GET /api/me: it returns the signed-in user, read from the
// database so the role is current even if it changed since sign-in. 401
// user_not_found if the token's user no longer exists.
func (a *API) me(w http.ResponseWriter, r *http.Request) {
	u, err := a.users.Get(r.Context(), identity.UserID(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.OK(w, u)
}
