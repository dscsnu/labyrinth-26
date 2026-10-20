// Package server wires the use cases together: it builds each one, registers
// its API, and serves the routes that span several of them (health and the
// live event stream).
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/atlas"
	"labyrinth/internal/auth"
	"labyrinth/internal/bosses"
	"labyrinth/internal/platform/config"
	"labyrinth/internal/platform/httpx"
	"labyrinth/internal/platform/identity"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/qr"
	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
	"labyrinth/internal/user"
)

// App is the running backend.
type App struct {
	Handler    http.Handler
	Hub        *live.Hub
	Atlas      *atlas.Atlas
	Tokens     *auth.Tokens
	LoginCodes *auth.LoginCodes
	Teams      *teams.Store
	TeamState  *teamstate.Store

	db     *postgres.DB
	stream *live.Streamer
}

// Build loads in-memory state from Postgres and assembles the HTTP routes.
// google is the Google endpoints to use (auth.DefaultGoogleEndpoints, or a
// fake in tests).
func Build(ctx context.Context, cfg *config.Config, db *postgres.DB, google auth.GoogleEndpoints, log *slog.Logger) (*App, error) {
	set := cfg.Settings
	hub := live.NewHub(set.Live.HubShards, set.Live.SubscriberBuffer)
	// One epoch for the whole process: every live.Version this backend
	// hands out (team, team_state) starts its seq again in it after a
	// restart, so clients never mistake a restart for going back.
	epoch := time.Now().UnixMilli()

	users := user.New(db)
	tokens := auth.NewTokens(cfg.JWTKey, set.Auth.TokenTTL.Duration)
	codes := auth.NewLoginCodes(db, set.Auth.LoginCodeTTL.Duration)
	g := auth.NewGoogle(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.PublicURL.String()+"/auth/google/callback", google)

	// The atlas is static content every game use case reads, so it loads first.
	theAtlas, err := atlas.Load(ctx, db, log, atlas.Rules{MaxPatternPoints: set.Game.MaxPatternPoints})
	if err != nil {
		return nil, err
	}
	// So are the bosses every team fights.
	theBosses, err := bosses.Load(ctx, db, log, bosses.Rules{Count: set.Game.Bosses})
	if err != nil {
		return nil, err
	}

	// stateStore is loaded after teams (it reads teams), but teams' OnStart
	// and OnRosterChange need it. Build assigns it before returning, and no team can start
	// before the routes are served, so OnStart never sees it nil.
	var stateStore *teamstate.Store
	teamStore, err := teams.Load(ctx, db, hub, log, teams.Rules{
		Size:          set.Teams.Size,
		NameMaxLength: set.Teams.NameMaxLength,
		WriteTimeout:  set.Storage.WriteTimeout.Duration,
		CodeAttempts:  set.Teams.CodeAttempts,
		// A started team's members get its (empty) state right after the
		// team, without reconnecting their stream.
		OnStart: func(t teams.Team) { stateStore.PublishTeam(t.ID) },
		// An admin removing a member from a started team: team state stops
		// sending to them and refuses their writes.
		OnRosterChange: func(t teams.Team) { stateStore.RosterChanged(t.ID) },
		Epoch:          epoch,
	})
	if err != nil {
		return nil, err
	}
	// A sign-in may change a name or avatar that teams holds in memory.
	authAPI := auth.NewAPI(cfg, g, tokens, codes, users, func(u user.User) {
		teamStore.RefreshProfile(u.ID, u.Name, u.AvatarURL)
	}, log)
	stateStore, err = teamstate.Load(ctx, db, theAtlas, theBosses, teamStore, hub, log, teamstate.Rules{
		MaxPatternPoints: set.Game.MaxPatternPoints,
		Health:           set.Game.Health,
		WriteTimeout:     set.Storage.WriteTimeout.Duration,
		Epoch:            epoch,
	})
	if err != nil {
		return nil, err
	}

	app := &App{Hub: hub, Atlas: theAtlas, Tokens: tokens, LoginCodes: codes, Teams: teamStore, TeamState: stateStore, db: db,
		stream: &live.Streamer{
			Heartbeat:    set.Live.Heartbeat.Duration,
			WriteTimeout: set.Live.WriteTimeout.Duration,
			RetryMin:     set.Live.RetryMin.Duration,
			RetryMax:     set.Live.RetryMax.Duration,
		}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", app.health)
	mux.HandleFunc("GET /openapi.yaml", openAPI)
	mux.HandleFunc("GET /docs", docs)
	authAPI.Routes(mux)
	userAPI := user.NewAPI(users, log)
	userAPI.Routes(mux, authAPI.Authed)
	// Admin routes: signed in, and ADMIN by the role in the database (never
	// the token's, which can be stale).
	admin := func(h http.HandlerFunc) http.Handler { return authAPI.RequireAuth(userAPI.RequireAdmin(h)) }
	// Staff routes: signed in, and HELPER or ADMIN by the role in the database.
	staff := func(h http.HandlerFunc) http.Handler { return authAPI.RequireAuth(userAPI.RequireStaff(h)) }
	teamsAPI := teams.NewAPI(teamStore, log)
	teamsAPI.Routes(mux, authAPI.Authed)
	teamsAPI.AdminRoutes(mux, admin)
	stateAPI := teamstate.NewAPI(stateStore, log)
	stateAPI.Routes(mux, authAPI.Authed)
	stateAPI.AdminRoutes(mux, admin)
	bosses.NewAPI(theBosses).Routes(mux, authAPI.Authed)
	qrAPI := qr.NewAPI(qr.New(cfg.QRSecret, qr.Rules{
		Rotate:    set.QR.Rotate.Duration,
		Tolerance: set.QR.Tolerance.Duration,
	}, theAtlas), log)
	qrAPI.Routes(mux, authAPI.Authed)
	qrAPI.StaffRoutes(mux, staff)
	// The one event stream. Browsers open it on the shared domain with the
	// labyrinth_token cookie (RequireStreamAuth); other clients use Bearer.
	mux.Handle("GET /api/events", authAPI.RequireStreamAuth(http.HandlerFunc(app.events)))

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

// events handles GET /api/events, the caller's one live stream. It starts
// with the snapshot and then sends one event per change, at every scope.
func (a *App) events(w http.ResponseWriter, r *http.Request) {
	userID := identity.UserID(r)
	sub := a.Hub.Subscribe(userID) // before reading snapshots, so no change slips between
	defer sub.Close()
	a.stream.Serve(w, r, sub, a.snapshot(userID))
}

// snapshot is everything a new stream starts with, in order: hello, then the
// user's and team's state (team or no_team, then team_state if in a team),
// then game-wide state.
func (a *App) snapshot(userID uuid.UUID) []live.Event {
	evs := []live.Event{hello(time.Now()), a.Teams.Snapshot(userID)}
	if ev, ok := a.TeamState.Snapshot(userID); ok {
		evs = append(evs, ev)
	}
	// Game-wide snapshots go here, after the user's own.
	return evs
}

// helloConnected is the type of the "hello" event: the stream is open.
const helloConnected live.EventType = "connected"

// hello is the "hello" event that opens every stream, of type connected: the
// server's clock, so clients can correct anything they show relative to now
// ("2 min ago") for their own clock's offset.
func hello(now time.Time) live.Event {
	return live.MustNewEvent("hello", helloConnected, struct {
		ServerTime time.Time `json:"server_time"`
	}{now.UTC()}) // can't fail: a struct of one time
}
