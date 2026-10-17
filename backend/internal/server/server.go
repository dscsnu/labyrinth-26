// Package server wires the use cases together: it builds each one, registers
// its API, and serves the routes that span several of them (health and the
// live event stream).
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"labyrinth/internal/auth"
	"labyrinth/internal/platform/config"
	"labyrinth/internal/platform/httpx"
	"labyrinth/internal/platform/identity"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
	"labyrinth/internal/user"
)

// App is the running backend.
type App struct {
	Handler    http.Handler
	Hub        *live.Hub
	Tokens     *auth.Tokens
	LoginCodes *auth.LoginCodes
	Teams      *teams.Store
	TeamState  *teamstate.Store

	db        *postgres.DB
	heartbeat time.Duration
}

// Build loads in-memory state from Postgres and assembles the HTTP routes.
// google is the Google endpoints to use (auth.DefaultGoogleEndpoints, or a
// fake in tests).
func Build(ctx context.Context, cfg *config.Config, db *postgres.DB, google auth.GoogleEndpoints, log *slog.Logger) (*App, error) {
	set := cfg.Settings
	hub := live.NewHub(set.Live.SubscriberBuffer)

	users := user.New(db)
	tokens := auth.NewTokens(cfg.JWTKey, set.Auth.TokenTTL.Duration)
	codes := auth.NewLoginCodes(db, set.Auth.LoginCodeTTL.Duration)
	g := auth.NewGoogle(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.PublicURL.String()+"/auth/google/callback", google)

	teamStore, err := teams.Load(ctx, db, hub, log, teams.Rules{
		Size:          set.Teams.Size,
		NameMaxLength: set.Teams.NameMaxLength,
		WriteTimeout:  set.Storage.WriteTimeout.Duration,
		CodeAttempts:  set.Teams.CodeAttempts,
	})
	if err != nil {
		return nil, err
	}
	// A sign-in may change a name or avatar that teams holds in memory.
	authAPI := auth.NewAPI(cfg, g, tokens, codes, users, func(u user.User) {
		teamStore.RefreshProfile(u.ID, u.Name, u.AvatarURL)
	}, log)
	stateStore, err := teamstate.Load(ctx, db, teamStore, hub, log, teamstate.Rules{
		ScanWindow:       set.Game.ScanWindow.Duration,
		ScanGrace:        set.Game.ScanGrace.Duration,
		MaxPatternPoints: set.Game.MaxPatternPoints,
		WriteTimeout:     set.Storage.WriteTimeout.Duration,
	})
	if err != nil {
		return nil, err
	}

	app := &App{Hub: hub, Tokens: tokens, LoginCodes: codes, Teams: teamStore, TeamState: stateStore, db: db,
		heartbeat: set.Live.Heartbeat.Duration}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", app.health)
	mux.HandleFunc("GET /openapi.yaml", openAPI)
	mux.HandleFunc("GET /docs", docs)
	authAPI.Routes(mux)
	user.NewAPI(users, log).Routes(mux, authAPI.Authed)
	teams.NewAPI(teamStore, log).Routes(mux, authAPI.Authed)
	teamstate.NewAPI(stateStore, log).Routes(mux, authAPI.Authed)
	mux.Handle("GET /api/teams/me/events", authAPI.Authed(app.events))

	app.Handler = http.MaxBytesHandler(envelopeUnrouted(mux), set.Server.MaxBodyBytes)
	return app, nil
}

// envelopeUnrouted answers requests no route matches in the JSON envelope:
// 404 not_found for an unknown path, 405 method_not_allowed (with the mux's
// Allow header) for a known path with the wrong method. Everything else,
// including the mux's redirects to canonical paths, goes to mux unchanged.
func envelopeUnrouted(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		// The mux's own answer is a plain-text 404 or 405; record it to learn
		// which, then send it as JSON.
		rec := &statusRecorder{header: http.Header{}}
		h.ServeHTTP(rec, r)
		switch rec.status {
		case http.StatusNotFound:
			httpx.ErrorCode(w, http.StatusNotFound, "not_found", "no such route")
		case http.StatusMethodNotAllowed:
			w.Header().Set("Allow", rec.header.Get("Allow"))
			httpx.ErrorCode(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		default: // not expected; let the mux answer as it would have
			mux.ServeHTTP(w, r)
		}
	})
}

// statusRecorder captures the status and headers the mux would send for an
// unrouted request, discarding its plain-text body.
type statusRecorder struct {
	header http.Header
	status int
}

// Header returns the recorded headers.
func (s *statusRecorder) Header() http.Header { return s.header }

// WriteHeader records the status.
func (s *statusRecorder) WriteHeader(status int) { s.status = status }

// Write discards the body, recording 200 if no status was set.
func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return len(b), nil
}

// health handles GET /healthz: 200 if Postgres is reachable, with the number
// of open streams.
func (a *App) health(w http.ResponseWriter, r *http.Request) {
	if err := a.db.Ping(r.Context()); err != nil {
		httpx.Error(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	httpx.OK(w, map[string]int{"streams": a.Hub.Count()})
}

// events handles GET /api/teams/me/events, the caller's live stream. It
// starts with their team ("team" or "no_team") and, if they're in a team, its
// state ("team_state"), then sends one event per change from either use case.
func (a *App) events(w http.ResponseWriter, r *http.Request) {
	userID := identity.UserID(r)
	sub := a.Hub.Subscribe(userID) // before reading snapshots, so no change slips between
	defer sub.Close()

	initial := []live.Event{a.Teams.Snapshot(userID)}
	if ev, ok := a.TeamState.Snapshot(userID); ok {
		initial = append(initial, ev)
	}
	live.Serve(w, r, sub, initial, a.heartbeat)
}
