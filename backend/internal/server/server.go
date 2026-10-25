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

	"labyrinth/internal/admin"
	"labyrinth/internal/atlas"
	"labyrinth/internal/auth"
	"labyrinth/internal/bosses"
	"labyrinth/internal/leaderboard"
	"labyrinth/internal/platform/config"
	"labyrinth/internal/platform/httpx"
	"labyrinth/internal/platform/identity"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/platform/stats"
	"labyrinth/internal/qr"
	"labyrinth/internal/qte"
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
	// Leaderboard broadcasts the top teams; main runs its loop (Run).
	Leaderboard *leaderboard.Leaderboard
	// QTEs holds the quick-time events and broadcasts the open one.
	QTEs *qte.Store

	db     *postgres.DB
	stream *live.Streamer

	// startedAt is when Build finished; the health route reports the uptime
	// since then.
	startedAt time.Time
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
	theBosses, err := bosses.Load(ctx, db, log, bosses.Rules{
		Count: set.Game.Bosses, AttackInterval: set.Game.BossAttack.Interval.Duration, AttackDamage: set.Game.BossAttack.BossDamage(),
	})
	if err != nil {
		return nil, err
	}

	// stateStore is loaded after teams (it reads teams), but teams' OnStart,
	// OnRosterChange and OnDelete need it. Build assigns it before returning,
	// and no team can start, lose a member or be deleted before the routes
	// are served, so no hook ever sees it nil.
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
		// An admin deleting a team: team state forgets it and its timer.
		OnDelete: func(teamID string) { stateStore.TeamDeleted(teamID) },
		Epoch:    epoch,
	})
	if err != nil {
		return nil, err
	}
	// A sign-in may change a name or avatar that teams holds in memory.
	authAPI := auth.NewAPI(cfg, g, tokens, codes, users, func(u user.User) {
		teamStore.RefreshProfile(u.ID, u.Name, u.AvatarURL)
	}, log)
	// The QR verifier teamstate uses to check scanned tokens; the same one
	// serves the qr use case's routes below.
	qrVerifier := qr.New(cfg.QRSecret, qr.Rules{
		Rotate:    set.QR.Rotate.Duration,
		Tolerance: set.QR.Tolerance.Duration,
	}, theAtlas)
	// QTEs, before team state, which checks answers against them.
	qteStore, err := qte.Load(ctx, db, hub, log, qte.Rules{
		AnswerWindow:    set.QTE.AnswerWindow.Duration,
		MaxAnswerWindow: set.QTE.MaxAnswerWindow.Duration,
		Powerups:        teamstate.PowerupIDs(),
		WriteTimeout:    set.Storage.WriteTimeout.Duration,
	})
	if err != nil {
		return nil, err
	}
	stateStore, err = teamstate.Load(ctx, db, theAtlas, theBosses, teamStore, qrVerifier, qteStore, hub, log, teamstate.Rules{
		MaxPatternPoints:  set.Game.MaxPatternPoints,
		Health:            set.Game.Health,
		Unlocks:           set.Game.UnlockSchedule(),
		QRCooldown:        set.Game.QRCooldown.Duration,
		PowerupMultiplier: set.Powerups.DamageMultiplier,
		PowerupHeart:      set.Powerups.Heart,
		ShortQRCooldown:   set.Powerups.ShortQRCooldown.Duration,
		ShortQRWindow:     set.Powerups.ShortQRWindow.Duration,
		WriteTimeout:      set.Storage.WriteTimeout.Duration,
		Epoch:             epoch,
		// A team that defeats its last boss is FINISHED (teams' write).
		OnFinished: func(teamID string) {
			if err := teamStore.Finish(context.Background(), teamID); err != nil {
				log.Error("mark team finished", "team_id", teamID, "error", err)
			}
		},
	})
	if err != nil {
		return nil, err
	}

	board := leaderboard.New(stateStore, teamStore, hub, log, leaderboard.Rules{
		Size: set.Leaderboard.Size, PublishEvery: set.Leaderboard.PublishEvery.Duration,
	})

	app := &App{Hub: hub, Atlas: theAtlas, Tokens: tokens, LoginCodes: codes, Teams: teamStore, TeamState: stateStore,
		Leaderboard: board, QTEs: qteStore, db: db, startedAt: time.Now(),
		stream: &live.Streamer{
			Heartbeat:    set.Live.Heartbeat.Duration,
			WriteTimeout: set.Live.WriteTimeout.Duration,
			RetryMin:     set.Live.RetryMin.Duration,
			RetryMax:     set.Live.RetryMax.Duration,
		}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", app.health)
	mux.HandleFunc("GET /openapi.yaml", openAPI)
	mux.HandleFunc("GET /docs", docs)
	authAPI.Routes(mux)
	// Every signed-in request goes through the user's token bucket
	// ([ratelimit]), after authentication so it's keyed by user: per user,
	// not per IP, since campus Wi-Fi puts thousands of phones behind a few
	// addresses.
	limiter := httpx.NewLimiter(set.RateLimit.PerSecond, set.RateLimit.Burst, set.RateLimit.Idle.Duration, nil)
	perUser := func(h http.Handler) http.Handler {
		return limiter.Middleware(func(r *http.Request) string { return identity.UserID(r).String() }, h)
	}
	authed := func(h http.HandlerFunc) http.Handler { return authAPI.RequireAuth(perUser(h)) }
	userAPI := user.NewAPI(users, log)
	userAPI.Routes(mux, authed)
	// Admin routes: signed in, and ADMIN by the role in the database (never
	// the token's, which can be stale).
	adminOnly := func(h http.HandlerFunc) http.Handler { return authAPI.RequireAuth(perUser(userAPI.RequireAdmin(h))) }
	// Staff routes: signed in, and HELPER or ADMIN by the role in the database.
	staff := func(h http.HandlerFunc) http.Handler { return authAPI.RequireAuth(perUser(userAPI.RequireStaff(h))) }
	teams.NewAPI(teamStore, log).Routes(mux, authed)
	teamstate.NewAPI(stateStore, log).Routes(mux, authed)
	leaderboard.NewAPI(board).Routes(mux, authed)
	qte.NewAPI(qteStore).Routes(mux, authed)
	// Every /api/admin/… route lives in the admin use case.
	admin.NewAPI(admin.New(teamStore, stateStore, qteStore, users, theAtlas, admin.Rules{UserSearchLimit: set.Admin.UserSearchLimit}), log).
		Routes(mux, adminOnly)
	bosses.NewAPI(theBosses).Routes(mux, authed)
	qrAPI := qr.NewAPI(qrVerifier, log)
	qrAPI.Routes(mux, authed)
	qrAPI.StaffRoutes(mux, staff)
	// The one event stream. Browsers open it on the shared domain with the
	// labyrinth_token cookie (RequireStreamAuth); other clients use Bearer.
	mux.Handle("GET /api/events", authAPI.RequireStreamAuth(perUser(http.HandlerFunc(app.events))))

	// Outermost first: the body limit, one access-log line per request (the
	// monitor's /health polls aside), then panic recovery, so a recovered
	// panic is logged as the 500 it became.
	app.Handler = http.MaxBytesHandler(
		httpx.AccessLog(log, httpx.Recover(log, envelopeUnrouted(mux)), "/health"),
		set.Server.MaxBodyBytes)
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

// health is the GET /health payload: how long the process has served, how
// many event streams are open, and the runtime and process stats.
type health struct {
	// UptimeMS is how long ago Build finished, in milliseconds.
	UptimeMS int64 `json:"uptime_ms"`
	// Streams is the number of open event streams, across all users.
	Streams int `json:"streams"`
	// Snapshot's fields (go_version, goroutines, cpu, memory, …) are
	// flattened into the same object.
	stats.Snapshot
}

// health handles GET /health: 200 with the process's runtime and resource
// stats if Postgres is reachable, 503 otherwise.
func (a *App) health(w http.ResponseWriter, r *http.Request) {
	if err := a.db.Ping(r.Context()); err != nil {
		httpx.Error(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	httpx.OK(w, health{
		UptimeMS: time.Since(a.startedAt).Milliseconds(),
		Streams:  a.Hub.Count(),
		Snapshot: stats.Collect(),
	})
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
// then game-wide state (the open QTE or null, then the leaderboard).
func (a *App) snapshot(userID uuid.UUID) []live.Event {
	evs := []live.Event{hello(time.Now()), a.Teams.Snapshot(userID)}
	if ev, ok := a.TeamState.Snapshot(userID); ok {
		evs = append(evs, ev)
	}
	// Game-wide snapshots, after the user's own.
	if ev, err := a.QTEs.Snapshot(); err == nil {
		evs = append(evs, ev)
	}
	if ev, err := a.Leaderboard.Snapshot(); err == nil {
		evs = append(evs, ev)
	}
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
