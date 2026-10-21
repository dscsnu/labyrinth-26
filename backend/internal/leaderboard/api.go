package leaderboard

import (
	"net/http"

	"labyrinth/internal/platform/httpx"
	"labyrinth/internal/platform/identity"
)

// API serves the leaderboard route.
type API struct {
	board *Leaderboard
}

// NewAPI returns the leaderboard API backed by l.
func NewAPI(l *Leaderboard) *API { return &API{board: l} }

// Routes registers the leaderboard route; authed wraps a handler in the
// auth middleware.
func (a *API) Routes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/leaderboard", authed(a.get))
}

// get handles GET /api/leaderboard: the top leaderboard.size teams and the
// caller's own team's entry (null if they have no started team), ranked now.
func (a *API) get(w http.ResponseWriter, r *http.Request) {
	httpx.OK(w, a.board.ForUser(identity.UserID(r)))
}
