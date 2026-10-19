package bosses

import (
	"net/http"

	"labyrinth/internal/platform/httpx"
)

// API serves the boss routes.
type API struct {
	bosses *Bosses
}

// NewAPI returns the boss API backed by b.
func NewAPI(b *Bosses) *API { return &API{bosses: b} }

// Routes registers the boss routes; authed wraps a handler in the auth
// middleware.
func (a *API) Routes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/game/bosses", authed(a.all))
}

// all handles GET /api/game/bosses: every boss in serial order, the same for
// every signed-in user and static while the backend runs. A team's progress
// through them is in its team_state.
func (a *API) all(w http.ResponseWriter, _ *http.Request) {
	httpx.OK(w, a.bosses.All())
}
