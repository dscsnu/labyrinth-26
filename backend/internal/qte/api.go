package qte

import (
	"net/http"

	"labyrinth/internal/platform/httpx"
)

// API serves the players' QTE route. The admin routes are the admin use
// case's (internal/admin), and answering is teamstate's.
type API struct {
	store *Store
}

// NewAPI returns the QTE API backed by store.
func NewAPI(store *Store) *API {
	return &API{store: store}
}

// Routes registers GET /api/qte; authed wraps a handler in the auth
// middleware.
func (a *API) Routes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/qte", authed(a.get))
}

// get handles GET /api/qte: the open QTE as players see it (no answer), or
// null when none is open: the same JSON as the "qte" event's message.
func (a *API) get(w http.ResponseWriter, r *http.Request) {
	httpx.OK(w, a.store.Public())
}
