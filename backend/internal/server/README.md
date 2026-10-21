# server

Wires the use cases together, serves the routes that span several of them (`/health`, the live event stream, and the API docs at `/openapi.yaml` and `/docs`), and holds the end-to-end tests. It has no rules of its own. `main.go` (in `backend/`) runs it: startup, the background sweep, and shutdown are documented here too.

Every number below that comes from `config.toml` is shown with its current value and its key, e.g. **25 s** (`live.heartbeat`). Change the file, not the code.

## Contents
1. [Concepts](#concepts)
2. [Startup and shutdown](#startup-and-shutdown)
3. [How each file works](#how-each-file-works)
4. [Where each config.toml value goes](#where-each-configtoml-value-goes)
5. [The request path](#the-request-path)
6. [The event stream](#the-event-stream)
7. [API](#api)
8. [Rules and cases](#rules-and-cases)
9. [Concurrency](#concurrency)
10. [Failure and recovery](#failure-and-recovery)
11. [Limits and how to change them](#limits-and-how-to-change-them)
12. [Not decided yet](#not-decided-yet)
13. [The test harness](#the-test-harness)
14. [API docs (OpenAPI)](#api-docs-openapi)
15. [Tests](#tests)

## Concepts
| Term | Meaning |
|---|---|
| **`App`** | What `Build` returns: the HTTP handler plus the pieces `main` and the tests need (`Hub`, `Atlas`, `Tokens`, `LoginCodes`, `Teams`, `TeamState`, `Leaderboard`). `Atlas` is the `*atlas.Atlas` (static content); nothing reads `App.Atlas` today (`Build` passes the atlas to teamstate directly), it's there for `main`, tests and future use cases. Unexported: `db`, the `*postgres.DB` (for `/health`); `stream`, the `*live.Streamer`; and `startedAt` (the uptime `/health` reports). |
| **Build order** | Each use case is constructed after everything it reads. `atlas` and `bosses` are static content the game use cases read, so `atlas.Load` and `bosses.Load` run first. `teamstate` reads `atlas`, `bosses`, `teams` and `qte`, and auth's sign-in hook calls `teams`, so `teams.Load` runs before both, and `qte.Load` before `teamstate.Load`. |
| **Authed route** | A route wrapped in `Build`'s `authed`: `authAPI.RequireAuth(perUser(h))`, auth's middleware then the per-user rate limit (passed to each use case's `Routes`). It needs `Authorization: Bearer <jwt>` and puts the caller on the request via `platform/identity`. `GET /api/events` uses `RequireStreamAuth` instead, which also takes the token from the `labyrinth_token` cookie. |
| **Stream** | One open `GET /api/events`: one `live.Subscription` in the hub, keyed by the caller's user ID. A client opens one, and it carries every scope. |
| **Scope** | Who an event goes to: one **user** (`Hub.Send`), a **team**'s current members (`Hub.Send` per member), or every stream, **game**-wide (`Hub.Broadcast`). The publisher decides; clients never subscribe to channels. |
| **Initial snapshots** | The events a stream sends before anything else: `hello`, then `team` or `no_team`, then `team_state` if the caller is in a team, then game-wide resources (today the `leaderboard`). |
| **Instance lock** | A Postgres advisory lock (key `7272027`) the process holds from just after connecting until it exits, checked every **5 s** (`storage.instance_lock_check`). A second backend, `migrate` or `seed` on the same database refuses to run. |
| **Unrouted request** | One no route pattern matches: an unknown path, or a known path with the wrong method. `envelopeUnrouted` answers it in the JSON envelope. |
| **Sign-in hook** | The `onSignIn` function `Build` gives auth: after every sign-in it calls `teams.Store.RefreshProfile`, so a renamed user shows the new name in their team at once. |
| **Start hook** | The `teams.Rules.OnStart` function `Build` gives teams: after a team starts (and its `team` event is published) it calls `teamstate.Store.PublishTeam`, so every member's open stream gets the team's (empty) `team_state` at once. teams can't import teamstate (AGENTS.md §4), hence the callback. |
| **Roster hook** | The `teams.Rules.OnRosterChange` function `Build` gives teams: after an admin removes a member from a started team (and its `team`/`no_team` events are published) it calls `teamstate.Store.RosterChanged`, which drops teamstate's cached roster so `team_state` stops reaching the removed player. Same reason for a callback as the start hook. |
| **Admin route** | A route wrapped by `Build`'s `adminOnly` helper: `authAPI.RequireAuth(perUser(userAPI.RequireAdmin(h)))`. Staff routes (the QR mint) use `staff`, the same with `RequireStaff`. The token proves who the caller is; `RequireAdmin` reads their role from Postgres on every request (`403 not_admin` unless `ADMIN`). |

## Startup and shutdown
`main.go`'s `run` does these steps in order. Any error returned is logged as `fatal` and the process exits with status `1`.

| # | Step | Detail |
|---|---|---|
| 1 | Logger | `slog` text handler on stdout, also set as `slog.SetDefault` (for `httpx`, which has no logger of its own). |
| 2 | `keygen` subcommand | Runs **before** `.env` and config: prints a fresh `JWT_PRIVATE_KEY` / `JWT_PUBLIC_KEY` pair and a `QR_SECRET`, and exits. |
| 3 | Load `.env` | `godotenv.Load()` reads `./.env` if it exists. A missing file is fine (deployments set real environment variables). Variables already set are not overridden. Any other read error is fatal. |
| 4 | `pubkey` subcommand | Parses `JWT_PRIVATE_KEY`, prints its `JWT_PUBLIC_KEY`, exits. |
| 5 | `config.Load()` | Reads `.env` variables and `config.toml`; every problem is reported at once (see `internal/platform/README.md`). |
| 6 | Signal context | `signal.NotifyContext` on `SIGINT` and `SIGTERM`. Everything below uses this `ctx`. |
| 7 | `postgres.Connect` | Opens the pool and pings. Failure: `connect to database: …`. |
| 8 | `db.AcquireInstanceLock(ctx, storage.instance_lock_check)` | Takes the instance lock, for serving **and** for `migrate` and `seed`. Another backend holding it: `ErrAnotherInstance`, exit `1`, before anything is migrated or seeded. Deferred `lock.Release()`. |
| 9 | `db.Migrate` | Applies new migrations under the migration advisory lock (key `7272026`). An edited applied migration: `ErrMigrationEdited`, exit `1`. |
| 10 | `migrate` subcommand | Exits here. |
| 11 | `seed` subcommand | `db.Seed2026` (`seeds/2026.sql`, `2026_spells.sql`, `2026_bosses.sql`), logs `seeded 2026 content`, exits. Content is read at startup, which is why seeding needs the lock. |
| 12 | `server.Build` | Loads memory from Postgres and builds the routes (see [`server.go`](#servergo)). |
| 13 | Login code sweep | `go cleanupLoginCodes(...)`: every **10 m** (`auth.login_code_sweep`) deletes expired, unredeemed login codes. The first sweep is one interval after startup, not at startup. |
| 13b | Leaderboard loop | `go app.Leaderboard.Run(ctx)`: every **5 s** (`leaderboard.publish_every`) ranks the teams and broadcasts the top (`leaderboard`, type `updated`) if it changed. Ends with `ctx` (the signal or a lost instance lock). |
| 14 | `http.Server` | `Addr ":" + PORT` (default `3000`), `ReadHeaderTimeout` **10 s** (`server.read_header_timeout`), `ReadTimeout` **30 s** (`server.read_timeout`), `IdleTimeout` **2 m** (`server.idle_timeout`). No `WriteTimeout`: event streams are long-lived. `live.Serve` clears the read deadline once a stream starts. |
| 15 | Shutdown hooks | `srv.RegisterOnShutdown(app.Hub.Close)`, then `app.TeamState.Close` and `app.QTEs.Close`: no boss attack or QTE close timer fires once shutting down (it would publish to closed streams). |
| 16 | Serve | `ListenAndServe` in a goroutine, logging `listening addr=:3000`. |
| 17 | Wait | For a signal, for `lock.Lost()` to close, or for `ListenAndServe` to fail (e.g. port in use), whose error is returned at once. |

**Shutdown**, on `SIGINT`, `SIGTERM` or a lost instance lock:
1. On a lost lock, logs `instance lock lost; shutting down` (error). Then logs `shutting down`. In both cases `ctx` is cancelled (a lost lock calls `stop()`), so the sweep goroutine returns.
2. `srv.Shutdown` with a **10 s** (`server.shutdown_timeout`) deadline. It closes the listener, runs the hooks (`Hub.Close`, which closes every subscription channel, so every `live.Serve` returns; `TeamState.Close`, which stops every boss attack timer), then waits for in-flight requests. Attacks need no flush: they're derived, and the next start recomputes them.
3. Deferred, in this order: cancel the shutdown context, release the instance lock (`pg_advisory_unlock` on its own connection), close the pool, stop signal capture.
4. `run` returns `errors.Join(lockErr, Shutdown's error)`. Exit `0` after a signal if `Shutdown` returned `nil`. After a lost lock, always exit `1` with `lost the instance lock` (`postgres.ErrInstanceLockLost`). If in-flight requests outlast **10 s**, `Shutdown` returns `context deadline exceeded` and the process exits `1`.

**Why streams need the hook.** An SSE handler never returns on its own, and `Shutdown` waits for active connections. Without `Hub.Close`, every shutdown would hit the **10 s** deadline.

**Why in-flight writes finish.** Write-throughs are detached from the request and capped at **5 s** (`storage.write_timeout`), which is under the **10 s** shutdown deadline.

**Why the instance lock.** Team and game state live in this process's memory. A second process would load its own copy and diverge.

**Why the lock comes before migrating.** A second backend (e.g. a new deploy started before the old one stops) must not change the schema under a running one. `seed` changes content the running backend read at startup, so it needs the lock too.

**Why stop on a lost lock.** If the lock's connection dies (Postgres restart, network cut), Postgres frees the lock and another backend could start and load its own copy. Stopping is the only safe move; the process supervisor restarts it, and it takes the lock again.

### `cleanupLoginCodes`
- A `time.Ticker` at `auth.login_code_sweep`. Each tick calls `LoginCodes.DeleteExpired` (`DELETE FROM auth_code WHERE expires_at <= now()`).
- An error is logged as `delete expired login codes` (warn) unless `ctx` is already cancelled. The loop keeps going.
- Returns when `ctx` is done.
- Correctness doesn't depend on it: `Redeem` already rejects expired codes. It only keeps `auth_code` small.

## How each file works

### `server.go`
- **`App`**: see [Concepts](#concepts).
- **`Build(ctx, cfg, db, google, log)`** constructs everything in dependency order. `google` is `auth.DefaultGoogleEndpoints` in `main`, a fake in tests.
  1. `live.NewHub(live.hub_shards, live.subscriber_buffer)`: the shared hub. Every use case that publishes gets this one.
  2. `user.New(db)`: Postgres-only users.
  3. `auth.NewTokens(cfg.JWTKey, auth.token_ttl)`: the JWT signer and verifier.
  4. `auth.NewLoginCodes(db, auth.login_code_ttl)`.
  5. `auth.NewGoogle(GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET, PUBLIC_URL + "/auth/google/callback", google)`.
  6. `atlas.Load(ctx, db, log, atlas.Rules{MaxPatternPoints: game.max_pattern_points})`: reads and validates the static content (waypoints, shapes, patterns) once. First, because every game use case reads it. An error (`load atlas: …`, `invalid atlas: …`) is returned as is. See `internal/atlas/README.md`.
     Then `bosses.Load(ctx, db, log, bosses.Rules{Count: game.bosses, AttackInterval: game.boss_attack.interval, AttackDamage: game.boss_attack.BossDamage()})`: reads and validates the boss sequence once from the `boss` table (exactly serials 1…`game.bosses`, or an empty table), each boss's attack from `config.toml` (`[game.boss_attack]`). An error (`load bosses: …`, `invalid bosses: …`) is returned as is. See `internal/bosses/README.md`.
  7. `teams.Load(ctx, db, hub, log, teams.Rules{…, OnStart, OnRosterChange, OnDelete})`: reads every team into memory. An error is returned as is. `OnStart` is `func(t) { stateStore.PublishTeam(t.ID) }`, the start hook; `OnRosterChange` is `func(t) { stateStore.RosterChanged(t.ID) }`, the roster hook. `stateStore` is declared before this step and assigned in step 9, which is safe because no team can start, and no admin can remove anyone, before `Build` returns and routes are served. `OnDelete` is `func(id) { stateStore.TeamDeleted(id) }`: an admin's deletion drops the team's state and timer.
  8. `auth.NewAPI(cfg, google, tokens, codes, users, onSignIn, log)`, after teams because `onSignIn` is `func(u) { teamStore.RefreshProfile(u.ID, u.Name, u.AvatarURL) }`. It reads `auth.oauth_state_ttl` and `SecureCookies()` from `cfg` itself.
  9. `qr.New(QR_SECRET, qr.Rules{Rotate: qr.rotate, Tolerance: qr.tolerance}, theAtlas)`: the QR verifier teamstate checks scanned tokens with; the same one serves the qr routes. Then `qte.Load(ctx, db, hub, log, qte.Rules{AnswerWindow: qte.answer_window, MaxAnswerWindow: qte.max_answer_window, Powerups: teamstate.PowerupIDs(), …})`: every QTE, re-arming the close timer of one still open. An error (`load qte: …`, or `qte N: …` for a stored kind or powerup it doesn't know) is returned as is. See `internal/qte/README.md`. Then `teamstate.Load(ctx, db, theAtlas, theBosses, teamStore, qrVerifier, qteStore, hub, log, teamstate.Rules{…})`: reads every team's state, checking it against the atlas and the bosses, applies any boss attacks that fell due while the backend was down, and arms every started team's attack timer. `Rules` gets `MaxPatternPoints`, `Health` (`game.health`), `LockedPatternPoints`, `UnlockAfterBosses`, `QRCooldown` (`game.qr_cooldown`), `WriteTimeout` and `Epoch`. It needs the atlas (`Atlas` interface), the bosses (`Bosses` interface) and `teamStore` (`Teams` interface, including `All`), so it runs after steps 6 and 7. Its `Rules.OnFinished` is `func(id) { teamStore.Finish(ctx, id) }` (an error is logged): a cast that defeats the last boss marks the team `FINISHED`; teamstate calls it after releasing its own lock. `Load` itself calls it for a team found with every boss defeated but still `IN_GAME`.
     Then `leaderboard.New(stateStore, teamStore, hub, log, leaderboard.Rules{Size: leaderboard.size, PublishEvery: leaderboard.publish_every})`: derives the ranking from memory; `main` starts its loop (`go app.Leaderboard.Run(ctx)`).
  10. The `App` (with `Atlas: theAtlas`, `Leaderboard: board`, and a `live.Streamer` from `live.heartbeat`, `live.write_timeout`, `live.retry_min` and `live.retry_max`), then a `http.ServeMux` with, in this order:
     - `GET /health` (no auth)
     - `GET /openapi.yaml` and `GET /docs` (no auth; see [API docs](#api-docs-openapi))
     - `authAPI.Routes(mux)`: the three `/auth/…` routes (no auth)
     - `user`, `teams`, `teamstate`, `leaderboard`, `bosses`, `qr`: each `NewAPI(…).Routes(mux, authed)` (`atlas` has no routes), where `authed(h)` is `authAPI.RequireAuth(perUser(h))` and `perUser` is the `[ratelimit]` limiter (`httpx.NewLimiter(ratelimit.per_second, ratelimit.burst, ratelimit.idle)`) keyed by the signed-in user's ID
     - the staff route: `staff := func(h) { return authAPI.RequireAuth(perUser(userAPI.RequireStaff(h))) }`, then `qrAPI.StaffRoutes(mux, staff)` (`GET /api/qr/{waypoint_id}`)
     - the admin routes: `adminOnly := func(h) { return authAPI.RequireAuth(perUser(userAPI.RequireAdmin(h))) }`, then `admin.NewAPI(admin.New(teamStore, stateStore, users, admin.Rules{UserSearchLimit: admin.user_search_limit}), log).Routes(mux, adminOnly)`: every `/api/admin/…` route, in the admin use case ([`internal/admin/README.md`](../admin/README.md))
     - `GET /api/events`, wrapped in `authAPI.RequireStreamAuth(perUser(…))` (Bearer or the `labyrinth_token` cookie; opening a stream takes a token like any request)
  11. `App.Handler = http.MaxBytesHandler(httpx.AccessLog(log, httpx.Recover(log, envelopeUnrouted(mux)), "/health"), server.max_body_bytes)`.
- **`envelopeUnrouted(mux)`** wraps the mux. For each request it asks `mux.Handler(r)` for the matching pattern:
  - a pattern matched → `mux.ServeHTTP` as usual
  - no pattern → runs the mux's own fallback handler into a **`statusRecorder`** (records status and headers, discards the plain-text body), then:
    - `404` → `httpx.ErrorCode(404, "not_found", "no such route")`
    - `405` → copies the mux's `Allow` header, then `httpx.ErrorCode(405, "method_not_allowed", "method not allowed")`
    - anything else → handled by the mux as it would have been (not expected: the mux's redirects have a pattern and never reach this branch)
- **`statusRecorder`**: `Header()`, `WriteHeader(status)` and `Write` (discards the body, sets `200` if no status yet) for the above.
- **`health`** handles `GET /health`: `db.Ping`; on error `503` `database unreachable`; otherwise `200` with the uptime, the open stream count and the process's runtime and resource stats (see [API](#api)).
- **`events`** handles `GET /api/events`: see [The event stream](#the-event-stream). It lives here because it carries events from every use case.
- **`snapshot(userID)`** is the initial snapshots, in order: `hello`, the user's `team` or `no_team`, their team's `team_state` (if any), then the game-wide ones: today the `leaderboard` (`Leaderboard.Snapshot()`, type `snapshot`).
- **`hello(now)`** is the `hello` event: `{"server_time": now}` in UTC. Clients measure their clock offset from it; it carries nothing else (game rules come from `GET /api/game/shapes`).

### `*_test.go`
End-to-end tests for every use case, because they need the whole app. See [The test harness](#the-test-harness).

## Where each config.toml value goes
| Key | Value | Passed to |
|---|---|---|
| `teams.size` | 6 | `teams.Rules.Size` |
| `teams.name_max_length` | 32 | `teams.Rules.NameMaxLength` |
| `teams.code_attempts` | 10 | `teams.Rules.CodeAttempts` |
| `game.max_pattern_points` | 6 | `atlas.Rules.MaxPatternPoints` and `teamstate.Rules.MaxPatternPoints` |
| `game.bosses` | 6 | `bosses.Rules.Count` |
| `game.health` | 10 | `teamstate.Rules.Health` |
| `game.locked_pattern_points` | 5 | `teamstate.Rules.LockedPatternPoints` |
| `game.unlock_after_bosses` | 5 | `teamstate.Rules.UnlockAfterBosses` |
| `game.qr_cooldown` | 5 m | `teamstate.Rules.QRCooldown` |
| `powerups.damage_multiplier`, `powerups.heart` | 2, 1 | `teamstate.Rules.PowerupMultiplier`, `PowerupHeart` |
| `powerups.short_qr_cooldown`, `powerups.short_qr_window` | 2 m, 5 m | `teamstate.Rules.ShortQRCooldown`, `ShortQRWindow` |
| `game.boss_attack.interval`, `game.boss_attack.damage` | 10 m; 1–6 by serial | `bosses.Rules.AttackInterval`, `AttackDamage` (`BossDamage()`) |
| `qr.rotate`, `qr.tolerance` | 5 s, 5 s | `qr.Rules.Rotate`, `Tolerance` |
| `auth.token_ttl` | 12 h | `auth.NewTokens` |
| `auth.login_code_ttl` | 2 m | `auth.NewLoginCodes` |
| `auth.oauth_state_ttl` | 10 m | read by `auth.API` from `cfg.Settings` (state token TTL and cookie `MaxAge`) |
| `auth.login_code_sweep` | 10 m | `main.cleanupLoginCodes` |
| `live.heartbeat` | 25 s | `live.Streamer.Heartbeat` |
| `live.subscriber_buffer` | 16 | `live.NewHub` |
| `live.hub_shards` | 64 | `live.NewHub` |
| `live.write_timeout` | 10 s | `live.Streamer.WriteTimeout` |
| `live.retry_min`, `live.retry_max` | 1 s, 6 s | `live.Streamer.RetryMin`, `RetryMax` |
| `server.read_header_timeout` | 10 s | `http.Server.ReadHeaderTimeout` (`main`) |
| `server.read_timeout` | 30 s | `http.Server.ReadTimeout` (`main`); cleared per stream by `Streamer.Serve` |
| `server.idle_timeout` | 2 m | `http.Server.IdleTimeout` (`main`) |
| `server.shutdown_timeout` | 10 s | the `srv.Shutdown` context (`main`) |
| `server.max_body_bytes` | 1,048,576 | `http.MaxBytesHandler` |
| `storage.write_timeout` | 5 s | `teams.Rules.WriteTimeout` and `teamstate.Rules.WriteTimeout` |
| `storage.instance_lock_check` | 5 s | `db.AcquireInstanceLock` (`main`) |
| `leaderboard.size`, `leaderboard.publish_every` | 10, 5 s | `leaderboard.Rules.Size`, `PublishEvery` |
| `admin.user_search_limit` | 20 | `admin.Rules.UserSearchLimit` |
| `ratelimit.per_second`, `ratelimit.burst`, `ratelimit.idle` | 10, 40, 10 m | `httpx.NewLimiter`, the `perUser` wrapper |

From `.env` (via `config.Config`): `JWT_PRIVATE_KEY` → `auth.NewTokens`; `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `PUBLIC_URL` → `auth.NewGoogle`; `FRONTEND_URL`, `ALLOWED_EMAIL_DOMAINS` → `auth.NewAPI` (via `cfg`); `QR_SECRET` → `qr.New`; `DATABASE_URL` and `PORT` → `main`.

## The request path
Outermost first:

```
http.Server              ReadHeaderTimeout 10 s, ReadTimeout 30 s, IdleTimeout 2 m
└ http.MaxBytesHandler    every body capped at 1,048,576 bytes (server.max_body_bytes)
  └ httpx.AccessLog       one line per finished request (not /health)
    └ httpx.Recover       a panic → logged with its stack, 500 internal error
      └ envelopeUnrouted  unmatched → 404 not_found, wrong method → 405 method_not_allowed (JSON)
        └ http.ServeMux   Go 1.22 method patterns
          └ RequireAuth   only on authed routes: Bearer JWT → identity on the context
            └ perUser     the user's token bucket: over it → 429 rate_limited, Retry-After
              └ handler   use case api.go, or health / events
```

- **Body limit.** `MaxBytesHandler` replaces `r.Body` with an `http.MaxBytesReader`. Reading past the limit fails with `*http.MaxBytesError`. Every handler reads bodies with `httpx.DecodeJSON`, so an oversized body surfaces as a decode error: `400 invalid_body` in teams, teamstate and `POST /auth/token`. It applies to every route, including `/auth/…` and `/health`, but only matters where a body is read.
- **Read timeout.** A client has **30 s** (`server.read_timeout`) from accepting the connection to finish sending a request, body included, so a trickled body can't hold a connection open. Event streams clear it when they start.
- **Routing before auth.** The mux matches first, so an unknown path is `404 not_found` and a wrong method is `405 method_not_allowed` (with `Allow`) even without a token, both in the JSON envelope:
  ```
  GET /api/nope                 → 404 {"success":false,"code":"not_found","message":"no such route"}
  DELETE /api/teams             → 405 Allow: POST
                                      {"success":false,"code":"method_not_allowed","message":"method not allowed"}
  ```
- **Auth.** `RequireAuth` (in `internal/auth`; `RequireStreamAuth` is the same, but with no header it reads the token from the `labyrinth_token` cookie):
  - no header, a scheme other than `Bearer` (case-insensitive), or an empty token → `401 missing_token`, `WWW-Authenticate: Bearer`
  - a token that fails `VerifyAccess` (bad signature, expired, malformed) → `401 invalid_token`, `WWW-Authenticate: Bearer error="invalid_token"`
  - otherwise the handler runs with `identity.Identity{UserID, Email, Name, Role}` from the token's claims
- **Access log.** `httpx.AccessLog` writes one `INFO` line `request` per request when it finishes: `route` (the mux pattern that matched, e.g. `POST /api/teams/me/state/scans`, so IDs in paths don't make every line unique; the method and path for an unrouted one), `status`, `bytes`, `ms`. An event stream is logged when it closes, with its whole duration. `/health` isn't logged (monitors poll it). Its recorder implements `Unwrap`, so `http.ResponseController` (the stream's `Flush` and write deadlines) reaches the real writer.
- **Panic recovery.** `httpx.Recover` catches a panic in any handler: it logs `panic serving request` at `ERROR` with the method, path, value and stack, and answers `500 {"success":false,"message":"internal error"}` if nothing was written yet (otherwise the response is left as it is; the client sees it cut short). `http.ErrAbortHandler` is re-panicked: net/http uses it to abort a response on purpose. It sits inside the access log, so the line shows the 500.
- **Rate limit.** `perUser` runs after `RequireAuth`, so it's keyed by the signed-in user, not the IP (campus Wi-Fi puts thousands of phones behind a few addresses). Each user has a token bucket of **10** requests a second (`ratelimit.per_second`) in bursts of up to **40** (`ratelimit.burst`): every signed-in route counts, admin routes and opening `/api/events` included (a stream costs one token when it opens, nothing while it's open). Over it: `429 rate_limited` with `Retry-After` (whole seconds, at least 1), and the handler doesn't run. Unauthenticated routes (`/health`, `/auth/…`, docs) aren't limited: the Google round trip and the single-use login code bound `/auth`. Buckets idle for **10 min** (`ratelimit.idle`) are dropped; memory is one small bucket per active user.
- No CORS at this layer: the SvelteKit server calls the API itself, and the browser reaches only `/api/events`, on the frontend's origin through the proxy (see the backend README, "Deploying").

## The event stream
The complete reference for the live stream: what it carries, what triggers each event, and the guarantees a client can rely on. AGENTS.md §9 and §17 hold the rules for publishing.

### Endpoint
| | |
|---|---|
| Backend | `GET /api/events`, with `Authorization: Bearer <jwt>` or, failing that, the `labyrinth_token` cookie (any signed-in user, in a team or not). No token or a bad one: `401 missing_token` / `401 invalid_token`, no stream. |
| Production routing | The reverse proxy in front of both apps sends `/api/events` on the frontend's origin straight to the backend, so the browser's cookie reaches it and no Node process copies the stream. Deployment config doesn't exist yet; this is the rule it must follow |
| Browser | `new EventSource("/api/events")` on the frontend's own origin (`/team`, `/play`). The browser sends the `labyrinth_token` cookie (httpOnly, `path=/`, `SameSite=Lax`). In local dev, Vite's dev server forwards `/api/events` to `BACKEND_URL` (`frontend/vite.config.ts`, `server.proxy`), standing in for the reverse proxy. There is no SvelteKit route for the stream |
| Response | `200`, `Content-Type: text/event-stream`, `Cache-Control: no-store`, `X-Accel-Buffering: no` (so nginx doesn't buffer). The body never ends on its own |

### How `events` serves a stream
1. `userID := identity.UserID(r)`.
2. **`sub := a.Hub.Subscribe(userID)`**, then `defer sub.Close()`.
3. **Initial snapshots** (`snapshot`), read from memory after subscribing:
   - `hello` of type `connected`: `{"server_time": …}`, the server's clock: the one way clients measure time against the game (see [Client integration](#client-integration), step 5).
   - `a.Teams.Snapshot(userID)`: `team` of type `snapshot`, its message the same JSON as `GET /api/teams/me`; or `no_team` of type `snapshot` with `{}` if the user is in no team (or the team failed to encode, which is logged).
   - `a.TeamState.Snapshot(userID)`: if the user is in a team, `team_state` of type `snapshot`, its message the same JSON as `GET /api/teams/me/state`. Skipped if not in a team (or on an encode error, logged). Sent for a `LOBBY` team too (an empty state).
   - Game-wide resources come last: `a.QTEs.Snapshot()`, `qte` of type `snapshot` (the open QTE without its answer, or `null`), then the `leaderboard` (type `snapshot`).
4. `a.stream.Serve(w, r, sub, initial)` (`live.Streamer`): clears the read deadline, writes a `retry:` line and the initial events in one flush, then every event from `sub`, plus `: ping` after **25 s** (`live.heartbeat`) with no event. Returns when the client disconnects, a write fails or takes over **10 s** (`live.write_timeout`), the subscription is dropped or the hub closes.
5. The deferred `sub.Close()` removes the subscription; the `/health` count drops.

**The stream follows the user.** Subscriptions are keyed by user ID, not team. Joining, leaving or a new team all arrive on the same stream (`TestTeamEventStream`). Each tab is its own subscription and gets every event.

### Scopes: how an event finds its streams
Nobody picks a channel. The publisher decides who gets each event, at the moment it sends it:

| Scope | Sent with | To | Today |
|---|---|---|---|
| user | `Hub.Send(userID, ev)` | that user's streams | `no_team` |
| team | `Hub.Send` for each member | the team's members at that moment | `team`, `team_state` |
| game | `Hub.Broadcast(ev)` | every open stream | `leaderboard`, `qte` |

- **Team membership is read when sending,** so a user who joins a team gets its next event without reconnecting. `teams` already holds the team it publishes; `teamstate` caches an `IN_GAME` team's frozen roster in its entry on the first publish, so publishing reads nothing outside the entry.
- **Why not topics clients subscribe to.** Every join or leave would need a resubscribe, an event could fall in the gap, and a client could ask for a topic it shouldn't see.
- **Each event is encoded once** (`live.NewEvent`): the team's members, or every stream for a broadcast, share the same bytes.

### Wire format
Each event is exactly `event: <name>\n` + `data: <envelope as compact JSON on one line>\n` + `\n`. The envelope is described in [Event format](#event-format). A heartbeat is the comment `: ping\n\n`, which `EventSource` ignores. The stream opens with `retry: <ms>\n\n`, a reconnect delay picked at random per stream between **1 s** (`live.retry_min`) and **6 s** (`live.retry_max`). No `id:` fields are sent. Events already queued when one is written go out with it in a single flush; every write has a **10 s** deadline (`live.write_timeout`).

Byte-level example for a user in the lobby team `482913` with two members, from connect to the first ping (`␤` = `\n`; the epoch is the backend's start time):
```
HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-store
X-Accel-Buffering: no

retry: 3412␤
␤
event: hello␤
data: {"type":"connected","message":{"server_time":"2026-10-17T11:58:30.123456Z"}}␤
␤
event: team␤
data: {"type":"snapshot","message":{"id":"482913","name":"Scanners","leader_id":"3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b01","status":"LOBBY","created_at":"2026-10-17T11:50:00Z","started_at":null,"members":[{"id":"3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b01","name":"Asha","avatar_url":null,"is_ready":false,"joined_at":"2026-10-17T11:50:00Z"},{"id":"3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b02","name":"Bilal","avatar_url":"https://example.com/b.png","is_ready":true,"joined_at":"2026-10-17T11:51:30Z"}],"version":{"epoch":1760687400000,"seq":3}}}␤
␤
event: team_state␤
data: {"type":"snapshot","message":{"team_id":"482913","waypoints":[],"patterns":[],"shapes":[],"selection":null,"last_cast":null,"stack":[],"poisoned":false,"cast_count":0,"version":{"epoch":1760687400000,"seq":0}}}␤
␤
: ping␤
␤
```

### Event format
**Every event's `data` is an envelope:**
```json
{"type": "spell_cast", "message": { …the full team_state… }}
```
- The SSE **`event:` name is the resource**: `hello`, `team`, `no_team`, `team_state`, and the game-wide `qte` and `leaderboard`. `EventSource` dispatches on it (`addEventListener("team_state", …)`).
- **`type` is why it was sent**: the cause of the change (`member_joined`, `scan_added`, `spell_cast`, …), or one of the two types every resource shares: **`snapshot`** (the state a stream opens with, on every connect: not a change, the current truth) and **`resync`** (the state reloaded from Postgres after a write failed to persist: a correction, not an action).
- **`message` is always the resource's whole state**, never a diff: the same JSON as its GET route. Whatever the type, the newest `message` of each event name is the whole truth, so a client that misses an event, or reconnects, loses nothing.
- **How to use it:** a client that only renders state reads `message` and ignores `type`. A client that animates or notifies switches on `type` (a spell-cast animation on `spell_cast`, a "Bilal joined" toast on `member_joined`), and still renders from `message`. Never derive state from `type` alone.
- Each envelope is a schema in `openapi.yaml`: `HelloEvent`, `TeamEvent`, `NoTeamEvent`, `TeamStateEvent`, `QTEEvent`, `LeaderboardEvent`, with `type` as an enum, so client types can be generated from it.

| Event | `type` | Sent when | To | A client might |
|---|---|---|---|---|
| `hello` | `connected` | every connect, first | that stream | re-measure its clock offset |
| `team` | `snapshot` | connect | that stream | render the roster |
| | `created` | the user created the team | the creator | open the lobby |
| | `member_joined` | a player joined | members | toast "X joined" |
| | `member_left` | a member left (not the leader) | remaining members | toast "X left" |
| | `leader_changed` | the leader left, or an admin removed them; `leader_id` is the new leader | remaining members | toast "X now leads" |
| | `member_removed` | an admin removed a member (not the leader) | remaining members | toast "X was removed" |
| | `ready_changed` | a member's ready flag changed | members | update ready marks |
| | `started` | the leader started; `status` is `IN_GAME` (its `team_state` of type `game_started` follows) | members | go to the game screen |
| | `profile_updated` | a member signed in with a new name or avatar | members | re-render names |
| | `finished` | the team defeated the last boss: `status` is `FINISHED`; its game writes are `not_in_game` from now | members | show the win, end play |
| | `resync` | reloaded after a failed write | members | render |
| `no_team` | `snapshot` | connect, user in no team | that stream | show create/join |
| | `left` | the user left; the team carries on | that user | back to create/join |
| | `team_deleted` | the team is gone: the user was its last member and left, or an admin deleted it | that user (every member, for an admin's deletion) | back to create/join |
| | `removed` | an admin removed the user | that user | explain, back to create/join |
| | `resync` | a reload after a failed write found the user no longer in the team | that user | back to create/join |
| `team_state` | `snapshot` | connect, user in a team | that stream | render the game |
| | `game_started` | right after `team` of type `started`: the team's (empty) state | members | render the empty game |
| | `shape_selected` | the leader selected (or reselected) a shape; the stack was purged: a spell attempt starts | members | show the spell started |
| | `selection_cleared` | the leader cleared the selection | members | back to choosing |
| | `scan_added` | a scan joined the stack | members | show who scanned (`stack[].user_id`) |
| | `wrong_scan` | a scan joined the stack marked `wrong`; `poisoned` is `true` | members | tell the team, offer a reselect (never with the 2026 content alone: every checkpoint scan fits) |
| | `spell_cast` | a scan completed a pattern: `last_cast` is the new cast, `cast_count` went up, selection and stack purged, its `dealt` damage went to the current `boss` | members | play the cast, prepend `last_cast` to the log |
| | `boss_defeated` | a `spell_cast` whose damage defeated the boss: `bosses_defeated` went up, `boss` is the next one at 0 damage (or `null` after the last), whose attack clock starts now | members | celebrate the defeat, as well as everything `spell_cast` does |
| | `boss_attacked` | **with nobody acting**: the current boss's attack interval passed (one or more attacks): `health` went down, `boss.attacks` up, `boss.next_attack_at` moved on | members | show the hit, restart the countdown |
| | `knocked_out` | a `boss_attacked` that brought `health` to 0: `knocked_out` is `true`, `next_attack_at` `null`; selecting and scanning are refused | members | show the knocked-out banner, disable play |
| | `qr_cooldown_ended` | **with nobody acting**: the QR cooldown of one or more waypoints ended: they left `qr_cooldowns` and can be scanned again. An attack due at the same moment sends `boss_attacked` / `knocked_out` instead, carrying both changes | members | re-enable those waypoints |
| | `revived` | an admin revived the team: `health` is `game.health` again and the boss's attack clock restarted | members | clear the banner, enable play |
| | `cooldowns_reset` | an admin reset the team's cooldowns: `cooldown_class` null, `qr_cooldowns` empty | members | re-enable the types and points |
| | `powerup_granted` | an admin granted the team a powerup: `powerups.held` gained it | members | show it in the inventory |
| | `powerup_activated` | the leader used a `damage_multiplier`, `heart` or `short_qr_cooldown`: `powerups.held` lost it, `powerups.active` gained it (or `health` went up) | members | show the effect |
| | `powerup_ended` | **with nobody acting**: a `short_qr_cooldown`'s window ended (`powerups.active` lost it). An attack due at the same moment sends `boss_attacked` / `knocked_out` instead | members | end the effect |
| | `qte_answered` | a member answered the open quiz: `qte` is the answer (`correct`, `granted`), `powerups.held` gained the powerup if granted | members | show right or wrong, mark the quiz answered |
| | `resync` | reloaded after a failed write | members | render |

| `qte` | `snapshot` | connect, after the user's own events: the open QTE (`quiz.correct_option` null), or `null` | that stream | show the quiz, or nothing |
| | `started` | an admin started a QTE: open until `ends_at` | **every stream** (game scope, `Hub.Broadcast`) | show the quiz and count down to `ends_at` |
| | `closed` | **with nobody acting** at `ends_at`, or an admin ended it: the QTE once more, `status` `closed`, `quiz.correct_option` revealed. From now on the resource is `null` | every stream | reveal the answer, then hide the quiz |
| | `resync` | reloaded after a failed write: the open QTE or `null` | every stream | render |
| `leaderboard` | `snapshot` | connect, last (after the user's own events) | that stream | render the top |
| | `updated` | the top `leaderboard.size` (10) changed, checked every **5 s** (`leaderboard.publish_every`) | **every stream** (game scope, `Hub.Broadcast`) | re-render the top |

`no_team` of any type means: clear both the team and its team state.

### Events
| Event | `message` | Same JSON as | Envelope schema (`openapi.yaml`) |
|---|---|---|---|
| `hello` | `{"server_time": …}`, the server's clock as the stream opened; clients re-measure their clock offset from it on every connect | none | `HelloEvent` (`Hello`) |
| `team` | the user's team | `GET /api/teams/me` | `TeamEvent` (`Team`) |
| `no_team` | `{}` | (`GET /api/teams/me` is `404 not_in_team`) | `NoTeamEvent` |
| `team_state` | the user's team's game state, the `View` | `GET /api/teams/me/state` | `TeamStateEvent` (`TeamState`) |
| `qte` | the open quick-time event as players see it, or `null` | `GET /api/qte` | `QTEEvent` (`QTE`) |

The JSON below is each event's `message`.

**`team`**, an `IN_GAME` team of **6** (`teams.size`), members in join order (field meanings: `internal/teams/README.md`):
```json
{
  "id": "482913",
  "name": "Scanners",
  "leader_id": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b01",
  "status": "IN_GAME",
  "created_at": "2026-10-17T11:50:00Z",
  "started_at": "2026-10-17T11:58:00Z",
  "members": [
    {"id": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b01", "name": "Asha",   "avatar_url": null, "is_ready": false, "joined_at": "2026-10-17T11:50:00Z"},
    {"id": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b02", "name": "Bilal",  "avatar_url": "https://example.com/b.png", "is_ready": true, "joined_at": "2026-10-17T11:51:30Z"},
    {"id": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b03", "name": "Chen",   "avatar_url": null, "is_ready": true, "joined_at": "2026-10-17T11:52:10Z"},
    {"id": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b04", "name": "Divya",  "avatar_url": null, "is_ready": true, "joined_at": "2026-10-17T11:53:00Z"},
    {"id": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b05", "name": "Emeka",  "avatar_url": null, "is_ready": true, "joined_at": "2026-10-17T11:54:45Z"},
    {"id": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b06", "name": "Farah",  "avatar_url": null, "is_ready": true, "joined_at": "2026-10-17T11:56:20Z"}
  ],
  "version": {"epoch": 1760687400000, "seq": 12}
}
```
The leader can't set `is_ready`, and start ignores the leader's flag. Emails are never included.

**`no_team`**: always `{}`. The user is in no team; its `type` says why.

**`team_state`**, the same team after it cast the quadrilateral `2001 2002 2007 2008` at 12:00:12 (Divya's scan), selected `triangle` at 12:05:00 (Farah; the quadrilateral's class is on cooldown), and scanned `2001` (Asha, 12:05:10), `2002` (Asha again, 12:05:20: one member may scan any number) and `2099` (Bilal, 12:09:40), which fits no triangle with the other two. This one arrived with type `wrong_scan`. (`2099` is a test waypoint in no pattern: with the 2026 content alone every checkpoint scan fits, so no wrong scan happens.)
```json
{
  "team_id": "482913",
  "waypoints": ["2001", "2002", "2007", "2008", "2099"],
  "patterns": [["2001", "2002", "2007", "2008"]],
  "shapes": ["quadrilateral"],
  "selection": {"shape": "triangle", "selected_by": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b06", "selected_at": "2026-10-17T12:05:00Z"},
  "last_cast": {
    "pattern": ["2001", "2002", "2007", "2008"],
    "shape": "quadrilateral",
    "class": "class_4",
    "cast_by": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b04",
    "cast_at": "2026-10-17T12:00:12Z",
    "already_completed": false,
    "damage": 274,
    "dealt": 274,
    "boss": 1,
    "defeated_boss": true
  },
  "stack": [
    {"waypoint_id": "2001", "user_id": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b01", "scanned_at": "2026-10-17T12:05:10Z", "wrong": false},
    {"waypoint_id": "2002", "user_id": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b01", "scanned_at": "2026-10-17T12:05:20Z", "wrong": false},
    {"waypoint_id": "2099", "user_id": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b02", "scanned_at": "2026-10-17T12:09:40Z", "wrong": true}
  ],
  "poisoned": true,
  "bosses_defeated": 1,
  "boss": {"serial": 2, "damage": 0, "phase_started_at": "2026-10-17T12:00:12Z", "attacks": 0, "next_attack_at": "2026-10-17T12:10:12Z"},
  "damage_multiplier": 1,
  "health": 10,
  "knocked_out": false,
  "qr_cooldowns": [],
  "powerups": {"held": ["heart"], "active": [{"kind": "damage_multiplier", "until": null}]},
  "cast_count": 1,
  "version": {"epoch": 1760687400000, "seq": 9}
}
```
| Field | Meaning |
|---|---|
| `team_id` | the team |
| `waypoints` | every waypoint the team ever had a scan accepted at, sorted |
| `patterns` | every completed pattern, each as its sorted waypoint IDs |
| `shapes` | every shape with at least one completed pattern, in atlas index order |
| `selection` | the shape being attempted, who selected it and when; `null` if none |
| `last_cast` | the newest cast: its pattern (sorted IDs), shape, who completed it, when, `already_completed` (the team had cast that pattern before, so progress didn't grow), and `damage` (what the cast spell deals). Kept until the next cast; a new selection or a clear keeps it. `null` if the team never cast (or only before migration `0007`) |
| `stack` | the attempt's scans, bottom (oldest) first, until it casts or the team selects again. No time limit: nothing leaves it with time |
| `stack[].wrong` | the scan's waypoint, with the scans below it, can't be part of any pattern of the selected shape that the team has unlocked. At most one, always the top |
| `poisoned` | a scan in `stack` is `wrong`: further scans are refused with `409 selection_poisoned` until the leader selects again, which purges the stack |
| `bosses_defeated` | how many bosses the team has defeated |
| `boss` | the boss being fought: `serial` (its name, HP, attacks in `GET /api/game/bosses`), `damage` dealt to it so far, `phase_started_at` (when the team began fighting it, or was last revived), `attacks` (how many times it has attacked since), `next_attack_at` (when it attacks next; `null` when knocked out). `null` once every boss is defeated |
| `damage_multiplier` | what each cast's damage is multiplied by (rounded) before it's dealt; 1 unless something sets it |
| `health` | the team's health: **10** (`game.health`) at the start, less every boss attack. Derived from the phase and the clock, so it drops with nobody acting |
| `knocked_out` | `health` is 0: selecting and scanning are refused (`409 knocked_out`) and nothing attacks until an admin revives the team |
| `unlocked_points` | the most waypoints a pattern the team can cast may have: **4** (`game.locked_pattern_points` − 1) until it has defeated **5** (`game.unlock_after_bosses`) bosses, then **6** (`game.max_pattern_points`). Changes in the `boss_defeated` event of the unlocking cast |
| `locked_shapes` | the shapes the team can't select yet (`409 shape_locked`): every one of their patterns is bigger than `unlocked_points`. With the 2026 seed `["hexagon", "pentagon"]` until the unlock, then `[]` |
| `cooldown_class` | the spell class the team can't cast now: its last cast's (`class_4` after a 4-point spell), until it casts a spell of another class. `null` before the first cast |
| `cooldown_shapes` | the shapes the team can't select because of the cooldown (`409 shape_on_cooldown`): every one of their unlocked patterns is of `cooldown_class`. With the 2026 seed (one size per shape), after a 4-point cast: `["quadrilateral"]` |
| `qr_cooldowns` | the waypoints the team can't scan now (`409 waypoint_on_cooldown`), each `{"waypoint_id", "until"}`: every waypoint of a spell it cast less than **5 min** (`game.qr_cooldown`) ago, until that cast's `cast_at` + `game.qr_cooldown`, sorted by `until`, then ID. After the quadrilateral at 12:00:12: `[{"waypoint_id": "2001", "until": "2026-10-17T12:05:12Z"}, …]` for 2001, 2002, 2007, 2008. Other waypoints stay open and selecting isn't affected. An entry leaves at its `until` with nobody acting (`qr_cooldown_ended`). `[]` when none |
| `powerups` | `{"held": [kinds], "active": [{"kind", "until"}]}`: the powerups the team holds (at most one of each: `damage_multiplier`, `heart`, `short_qr_cooldown`, `instant_cast`) and the effects running (an armed `damage_multiplier`, `until` null; a `short_qr_cooldown`, until its window ends). Casts carry `instant`, `boosted` and `short_qr_cooldown`. Used with `POST /api/teams/me/state/powerups/{kind}` (the leader) |
| `qte` | the team's latest quiz answer, `{"qte_id", "option", "correct", "granted", "answered_by", "answered_at"}`, or `null` before its first. Compare `qte_id` with the open `qte` event's `id` to show it answered |
| `cast_count` | how many casts the team's log holds, repeats included (see [Versioning](#versioning-and-the-cast-log)) |
| `version` | orders this team's `team_state` snapshots (see [Versioning](#versioning-and-the-cast-log)) |

Every field, rule and case: `internal/teamstate/README.md`, "API".

### Versioning and the cast log
`team` and `team_state` each carry **`version: {epoch, seq}`** (`live.Version`, schema `Version`). It exists because a client gets copies of the same resource from two places, the stream and the responses to its own writes, on different connections, so they can arrive in either order; and because, on connect, a change queued while the snapshot was read can arrive just after it (see [Ordering](#ordering)).

**The rule, per resource** (compare a team's `team` only with its `team`, its `team_state` only with its `team_state`):
- keep the incoming copy if its `epoch` differs from the one you hold (the backend restarted: the snapshot after a reconnect is the truth);
- otherwise keep it only if its `seq` is higher;
- an equal version is the same state (a snapshot, or a write response matching its event): ignore it.

**How it moves:**
- `seq` goes up by one on every change: every type except `snapshot`, plus `resync`, which continues the sequence (a reload never goes back). `game_started` repeats the state's current version (it resends, it doesn't change).
- Rejections and no-ops leave it alone: they publish nothing.
- `epoch` is the backend process's start time, in Unix milliseconds, the same for every resource. Version is memory-only: after a restart every resource starts again at `seq` 0 in the new epoch, which the first rule handles.
- Two numbers rather than one, because JavaScript can't hold integers above 2^53 exactly.
- The write responses carry it too: every teams route that returns a team, and every teamstate route (`GET` state, both selection routes, `ScanResult.state`).

**The cast log.** `team_state` carries only the newest cast (`last_cast`); the whole log is `GET /api/teams/me/casts`, newest first, because it grows all day and would otherwise ride on every scan. `cast_count` tells a client when its copy is incomplete:
1. Load the log once (`GET /api/teams/me/casts`).
2. On each `team_state` of type `spell_cast` (or a scan response with `pattern_completed`), prepend `last_cast` if it isn't already there (match on `cast_at`).
3. Whenever `cast_count` is more than the log's length (it missed casts while disconnected, or between loading the log and connecting), fetch the log again.

**Why `team` and `team_state` stay separate.** Different use cases own them (teams, teamstate), under different locks, and they change at different times: the roster in the lobby, gameplay in the game. Merging them would resend the roster on every scan, the hottest path, and make teams depend on teamstate (AGENTS.md §4). A client keeps both and joins them by the team ID (`team.id` = `team_state.team_id`).

### What publishes what
Every event below goes to **every open stream** of each listed user. "Members" means the team's members after the change. Each is published after the change is applied, while the writer still holds the entry lock.

| Trigger | Event: `type` | To |
|---|---|---|
| **Connect** (`GET /api/events`) | `hello: connected`, then `team: snapshot` (or `no_team: snapshot`), then `team_state: snapshot` if in a team, then `qte: snapshot` and `leaderboard: snapshot` | that stream only |
| teams `POST /api/teams` (create) | `team: created` | the creator (its only member) |
| teams `POST /api/teams/join` | `team: member_joined` | members, the joiner included |
| teams `POST /api/teams/leave`, others remain | `team: member_left`, or `team: leader_changed` if the leader left (new `leader_id`) | the remaining members |
| | `no_team: left` | the user who left |
| teams `POST /api/teams/leave`, last member | `no_team: team_deleted` | the user who left (the team is deleted) |
| teams `PUT /api/teams/me/ready`, flag changed | `team: ready_changed` | members |
| teams `PUT /api/teams/me/ready`, flag already that value | nothing (no-op) | |
| teams `POST /api/teams/me/start` | `team: started` (`status` `IN_GAME`, `started_at` set), then `team_state: game_started` (the team's state, empty: no selection, stack `[]`, no progress) through the start hook (`OnStart` → `teamstate.PublishTeam`) | members |
| admin `DELETE /api/admin/teams/{team_id}/members/{user_id}` | `team: member_removed`, or `team: leader_changed` if the leader was removed (new `leader_id`) | the remaining members |
| | `no_team: removed` | the removed user; for a started team, the roster hook (`OnRosterChange` → `teamstate.RosterChanged`) then stops `team_state` reaching them. It publishes nothing itself |
| Sign-in (`onSignIn` → `teams.RefreshProfile`), name or avatar changed | `team: profile_updated` | members |
| Sign-in, nothing changed or not in a team | nothing | |
| teams write fails to persist (`reconcile`) | `team: resync`, reloaded from Postgres | its members |
| | `no_team: resync` | users no longer in it (all old members if the team is gone) |
| teamstate `PUT /api/teams/me/state/selection` (select, reselect; the leader only) | `team_state: shape_selected` (stack purged) | members |
| teamstate `DELETE /api/teams/me/state/selection`, something selected (the leader only) | `team_state: selection_cleared` (selection `null`, stack `[]`) | members |
| teamstate `DELETE …/selection`, nothing selected | nothing (no-op) | |
| teamstate `POST /api/teams/me/state/scans`, outcome `scan_added` | `team_state: scan_added` | members |
| teamstate `POST …/scans`, outcome `wrong_scan` | `team_state: wrong_scan` (the scan marked `wrong`, `poisoned` `true`) | members |
| teamstate `POST …/scans`, outcome `pattern_completed` (a cast) | `team_state: spell_cast` (selection `null`, stack `[]`, new `last_cast`, `cast_count` + 1, `boss.damage` up by `last_cast.dealt`) | members |
| teamstate `POST …/scans`, a cast that brings the boss to its HP | `team_state: boss_defeated` (as `spell_cast`, plus `bosses_defeated` + 1 and `boss` the next one, or `null`) | members |
| teamstate's timer, **nobody acting**: a boss attack fell due | `team_state: boss_attacked` (`health` down, `boss.attacks` up), or `knocked_out` when `health` reached 0 | members |
| teamstate's timer, **nobody acting**: a QR cooldown ended (and no attack fell due at the same moment) | `team_state: qr_cooldown_ended` (those waypoints out of `qr_cooldowns`) | members |
| teamstate's timer, **nobody acting**: a `short_qr_cooldown` window ended (and no attack fell due at the same moment) | `team_state: powerup_ended` | members |
| any teamstate write (select, clear, scan, revive, cooldown reset) when an attack or a QR cooldown's end fell due that the timer hasn't applied yet | first `team_state: boss_attacked` / `knocked_out` / `qr_cooldown_ended` (catch-up), then the write's own event | members |
| admin `POST /api/admin/teams/{team_id}/revive` | `team_state: revived` (`health` back to `game.health`, a fresh attack clock) | members |
| admin `POST /api/admin/teams/{team_id}/cooldowns/reset` | `team_state: cooldowns_reset` (`cooldown_class` null, `qr_cooldowns` `[]`) | members |
| admin `POST /api/admin/teams/{team_id}/powerups/{kind}`, a kind not held | `team_state: powerup_granted`; a kind already held: nothing (wasted) | members |
| teamstate `POST /api/teams/me/state/powerups/{kind}` (the leader): `damage_multiplier`, `heart`, `short_qr_cooldown` | `team_state: powerup_activated` | members |
| teamstate `POST …/powerups/instant_cast` | `team_state: spell_cast` or `boss_defeated` (`last_cast.instant`) | members |
| teamstate `POST /api/teams/me/state/qte/{qte_id}/answer` (any member, once per team) | `team_state: qte_answered` | members |
| admin `POST /api/admin/qtes` with `start`, or `POST /api/admin/qtes/{qte_id}/start` | `qte: started` | every stream |
| admin `POST /api/admin/qtes/{qte_id}/end`, or the qte timer at `ends_at` (**nobody acting**) | `qte: closed` (the answer revealed) | every stream |
| admin `POST /api/admin/qtes` without `start` (a draft) | nothing | |
| qte write fails to persist (`reconcile`) | `qte: resync` | every stream |
| admin `DELETE /api/admin/teams/{team_id}` | `no_team: team_deleted`; teamstate forgets the team (`OnDelete` → `TeamDeleted`, nothing published) | every member |
| admin `PUT /api/admin/users/{user_id}/role` | nothing | |
| a cast that defeats the last boss (after its `team_state: boss_defeated`) | `team: finished` (`status` `FINISHED`, through teamstate's `OnFinished` → `teams.Finish`) | members |
| the leaderboard loop, every **5 s** (`leaderboard.publish_every`), the top changed | `leaderboard: updated` | every stream |
| teamstate write fails to persist (`reconcile`) | `team_state: resync`, reloaded from Postgres (nothing if the reload fails too) | members |
| Any rejection (`4xx`, `409 waypoint_on_cooldown` included) or `500` before persisting | nothing (except a catch-up applied before the rejection, which is its own change) | |

**Four things are published because time passed: a boss attack, a QR cooldown ending, a `short_qr_cooldown` powerup's window ending (`powerup_ended`), and a QTE's window ending (`qte: closed`, from the qte store's one timer; it writes nothing, since the stored `ends_at` already stops answers).** Every boss attacks every **10 min** (`game.boss_attack.interval`) of a team's phase, for its `game.boss_attack.damage`, and a cast's waypoints open again **5 min** (`game.qr_cooldown`) after it. Teamstate arms one timer per started team, for whichever is due first, and publishes `boss_attacked` (or `knocked_out`) or `qr_cooldown_ended` itself, with nobody acting (AGENTS.md §9: the server owns time). Health is derived from the phase and the clock, and `qr_cooldowns` from the cast log and the clock, so neither writes to Postgres. Several changes applied at once (a late timer, an attack and an expiry at the same moment, or changes that fell due while the backend was down, applied at `Load` and seen in the `snapshot`) are one event, typed by the attack if there is one. Everything else follows a request (or a start, or a reconcile).

The HTTP response to a write carries the same JSON as its event's `message` (`ScanResult.state` for a scan), with the same `version`, but on a different connection: a client may see either first, and keeps the one with the higher version (equal: the same state).

### Ordering
- **One stream is FIFO.** `Hub.Send` and `Hub.Broadcast` put events on the subscription's one channel, and `Serve` writes them in that order.
- **Game-wide events in order.** A game use case broadcasts while holding the entry lock of the state it shows, so its events reach every stream in the order they were applied. `Broadcast` takes the hub's shards one at a time, so two broadcasts from different locks may interleave differently on different streams. The leaderboard broadcasts under its own `mu` (one `Refresh` at a time), so its `updated` events reach every stream in order; the qte store broadcasts under its `mu`, so a QTE's `started` always precedes its `closed`.
- **Per team, in order.** Each use case publishes while holding the team's entry lock (`teams`' entry, or `teamstate`'s entry for writes and `reconcile`), so one team's `team` events arrive in the order they were applied, and so do its `team_state` events.
- **No order between `team` and `team_state`,** with one exception. They're published under different locks; a `team` and a `team_state` from changes at the same moment may arrive either way round. Each is still the full truth for its part. **The exception is a start:** teams calls the start hook after publishing the `IN_GAME` `team`, still holding the team's entry lock, so on every member's stream the start's `team` comes before its `team_state` (`TestStartPublishesTeamState`).

**Why subscribe first.** A change published between reading the snapshot and subscribing would be lost. Subscribing first means any change after the snapshot is also queued. The cost: a change that lands between subscribing and reading the snapshot is delivered **twice** (once inside the snapshot, once queued). Events are full snapshots, so a repeat is harmless.

**A brief step back is possible, and `version` hides it.** Writers apply a change before publishing it. If change X is published (queued) and change Y is applied before the snapshot is read, the client sees Y (snapshot), then X (queued), then Y (queued once published). The final state is always the newest, because each publish happens after subscribing; a client that follows the [version rule](#versioning-and-the-cast-log) drops X and never shows the step back. `team` and `team_state` snapshots are read separately, so they may also disagree briefly on connect; the change's own events follow and correct it.

### Heartbeat
After **25 s** (`live.heartbeat`) with no event, `Serve` writes `: ping\n\n` and flushes, so mobile networks and proxies don't drop the idle connection. Every event resets the timer, so a busy stream gets no pings. The first ping comes 25 s after the initial events. Keep `live.heartbeat` under any proxy's idle timeout.

### Slow clients
Each subscription buffers **16** (`live.subscriber_buffer`) unread events. `Hub.Send` and `Hub.Broadcast` never block: when an event arrives while 16 are unread, that subscription is **dropped**. `Serve` writes the 16 buffered events, then the response ends. The client reconnects and starts from fresh snapshots, so it loses nothing. Other streams, the same user's other tabs included, are unaffected.

A client that stops reading (a phone that lost signal without closing the connection) fills its TCP window; the next write then waits at most **10 s** (`live.write_timeout`) before `Serve` gives up and returns, freeing the goroutine.

### Reconnection
- The browser's `EventSource` reconnects by itself after any end of the response (drop, restart, network loss), after the stream's `retry:` delay: random per stream between **1 s** and **6 s** (`live.retry_min`, `live.retry_max`), so after a restart the clients come back spread over five seconds rather than in one burst.
- Nothing is replayed (no `id:`, so no `Last-Event-ID`). The **first events after each connect are full snapshots** (`hello`, `team`/`no_team`, `team_state`, `qte`, `leaderboard`), which is all a client needs. A phone that was offline when a quiz closed reconnects to `qte: null`: it never keeps showing a stale quiz.
- `/team` and `/play` show "reconnecting…" while `EventSource` is between connections (`onerror`), and "live ●" once it's open.

### Shutdown and restart
- On shutdown, `srv.Shutdown` runs `Hub.Close`, which closes every subscription: each `Serve` returns and its response ends. New subscriptions are refused (closed at once).
- After a restart, `Build` reloads memory (every team's stack, wrong scans included, and its whole cast log), so the state is exactly what it was. Clients reconnect and get snapshots of the reloaded state.

### Never sent
- Anything for a **rejection** (every `4xx`) or a **no-op** (ready unchanged, clear with nothing selected, a sign-in that changes nothing).
- The **cast log**: `team_state` carries only `last_cast` and `cast_count`; the log is `GET /api/teams/me/casts` (see [Versioning and the cast log](#versioning-and-the-cast-log)).
- **Diffs**, event IDs, or payload-less notifications: every event, whatever its `type`, carries the full state in `message`.
- Another team's state, or any member's email.
- `GET /api/game/shapes` (`Game`) and `GET /api/game/bosses` (the boss list): static while the backend runs; read once per page load. Static data never goes in events (AGENTS.md §9): `team_state.boss` carries only the serial and the damage dealt.

### Client integration
What a client must do to use the stream as designed.

**Where the frontend is.** `/team` and `/play` read the envelope (`envelope(schema)` in `frontend/src/lib/types/Live.type.ts`) and keep a copy only if it's newer (`isNewer`, step 3): `team` by `version` (or always when it's another team), `team_state` by `version` for stream events and write responses alike. `/play` prepends each new `last_cast`, re-fetches the log through `/play/casts` when `cast_count` is more than it holds (step 4), says "<boss> defeated!" on a `boss_defeated` event, "<boss> attacked! Health n/10" on `boss_attacked`, a knocked-out message on `knocked_out` and "Revived" on `revived`, and counts down to `boss.next_attack_at` on the `hello` clock. Step 1 isn't done yet: each page opens its own `EventSource`.

1. **One connection for the app.** Open a single `EventSource` when the user is signed in, in the root layout (`frontend/src/routes/+layout.svelte`), held in a shared module (for example `frontend/src/lib/live.svelte.ts`), instead of one per page. Pages read from that module; navigating between pages doesn't reconnect. (Not done yet: `/team` and `/play` each open their own.)
2. **Register events by name, unwrap the envelope.** One zod schema per event: `type` as an enum, `message` as the payload's schema. Keep the newest `message` of each. `no_team` clears both `team` and `team_state`. Game-wide events carry `message: null` when nothing is active (`qte` between quizzes).
3. **Keep the newest copy by `version`**, for `team` and for `team_state` separately, whether it came from the stream or a write response ([the rule](#versioning-and-the-cast-log)).
4. **Use `type` for moments, `message` for state.** Play the cast on `spell_cast` (and `boss_defeated`, which is a cast too), celebrate on `boss_defeated`, flash a hit on `boss_attacked`, show a banner on `knocked_out` and clear it on `revived`, re-enable waypoints on `qr_cooldown_ended`, show a quiz on `qte: started` and reveal its answer on `qte: closed` (never infer the close from `ends_at`), show right or wrong on `qte_answered`, show a powerup on `powerup_granted` / `powerup_activated` and end it on `powerup_ended` (or any state whose `qr_cooldowns` no longer lists them), toast on `member_joined`, offer a reselect on `wrong_scan`; render everything from `message`. On `spell_cast`, prepend `last_cast` to the cast log; when `cast_count` exceeds the log's length, re-fetch `GET /api/teams/me/casts`.
5. **Measure time with `hello` only** (`frontend/src/lib/game/serverClock.ts`, `ServerClock`). On every `hello`: offset = `Date.parse(message.server_time)` − localNow, where localNow = `performance.timeOrigin + performance.now()` (monotonic, never jumps with the wall clock). Server now = localNow + offset. Every reconnect re-measures it; before the first `hello` the offset is 0. The estimate runs behind the server by the delay `hello` took, never ahead. The game has no time limit on an attempt; `/play` uses the clock for "scanned 2 min ago" labels and for the countdown to `boss.next_attack_at`. The same goes for each `qr_cooldowns[].until` ("2001 opens in 2:10"). The countdowns only display: the attack arrives as `boss_attacked`, and a waypoint opening as `qr_cooldown_ended`, when the server applies it. A scan of a waypoint still in `qr_cooldowns` is `409 waypoint_on_cooldown`: show the countdown rather than retrying.
6. **Open `/api/events` on the frontend's origin.** In production the shared domain's reverse proxy routes it to the backend, which reads the `labyrinth_token` cookie; in dev, Vite's proxy does (`frontend/vite.config.ts`).
7. **Keep reconnection as is.** No catch-up logic: every connect starts with complete snapshots (type `snapshot`), and step 3 sorts out anything that arrives out of order.

**Example** (TypeScript with zod; `TeamSchema`, `TeamStateSchema` from `frontend/src/lib/types`):
```ts
import { z } from "zod";

const Version = z.object({ epoch: z.number(), seq: z.number() });
type Version = z.infer<typeof Version>;

// One envelope schema per event: why it was sent, and the full payload.
const envelope = <T extends z.ZodTypeAny, K extends [string, ...string[]]>(types: K, message: T) =>
    z.object({ type: z.enum(types), message });

const TeamEvent = envelope(["snapshot", "created", "member_joined", "member_left", "leader_changed",
    "member_removed", "ready_changed", "started", "profile_updated", "resync"], TeamSchema);
const TeamStateEvent = envelope(["snapshot", "game_started", "shape_selected", "selection_cleared",
    "scan_added", "wrong_scan", "spell_cast", "boss_defeated", "boss_attacked", "knocked_out",
    "qr_cooldown_ended", "cooldowns_reset", "revived", "powerup_granted", "powerup_activated", "powerup_ended", "resync"], TeamStateSchema);

// Keep an incoming copy if the backend restarted (new epoch) or it's newer.
const newer = (incoming: Version, held?: Version) =>
    !held || incoming.epoch !== held.epoch || incoming.seq > held.seq;

const events = new EventSource("/api/events");
events.addEventListener("team_state", (e) => {
    const { type, message } = TeamStateEvent.parse(JSON.parse(e.data));
    if (!newer(message.version, state?.version)) return; // stale or repeated
    state = message;                                     // render from message
    if (type === "spell_cast" || type === "boss_defeated") playCast(message.last_cast!);
    if (type === "boss_defeated") celebrate(message.last_cast!.boss!);
    if (type === "boss_attacked" || type === "knocked_out") flashHit(message.health);
    if (message.cast_count > casts.length) refetchCasts();
});
events.addEventListener("no_team", () => { team = null; state = null; });
```

## API
| Method | Path | Auth | Response |
|---|---|---|---|
| GET | `/health` | none | `200 {"success":true,"payload":{"uptime_ms":…,"streams":n,"go_version":"…","goroutines":…,"gomaxprocs":…,"num_cpu":…,"cpu":{"process_seconds":…,"load_average":{"1":…,"5":…,"15":…}},"memory":{…}}}`, or `503 {"success":false,"message":"database unreachable"}` |
| GET | `/api/events` | Bearer or `labyrinth_token` cookie | `200 text/event-stream` (see above) |
| GET | `/openapi.yaml` | none | `200 application/yaml`: the OpenAPI 3.1 spec |
| GET | `/docs` | none | `200 text/html`: Swagger UI over `/openapi.yaml` |

Routes registered here but owned by use cases (see their READMEs):

| Use case | Routes | Auth |
|---|---|---|
| `auth` | `GET /auth/google/login`, `GET /auth/google/callback`, `POST /auth/token` | none |
| `user` | `GET /api/me` (`401 user_not_found` if the token's user was deleted) | Bearer |
| `teams` | `GET /api/teams/me`, `POST /api/teams`, `POST /api/teams/join`, `POST /api/teams/leave`, `PUT /api/teams/me/ready`, `POST /api/teams/me/start` | Bearer |
| `bosses` | `GET /api/game/bosses` (every boss, serial 1 first; static) | Bearer |
| `teamstate` | `GET /api/game/shapes`, `GET /api/teams/me/state`, `GET /api/teams/me/casts` (the cast log, newest first), `PUT` and `DELETE /api/teams/me/state/selection`, `POST /api/teams/me/state/scans`, `POST /api/teams/me/state/powerups/{kind}`, `POST /api/teams/me/state/qte/{qte_id}/answer` | Bearer |
| `qte` | `GET /api/qte` (the open QTE without its answer, or `null`) | Bearer |
| `leaderboard` | `GET /api/leaderboard` (the top `leaderboard.size` and the caller's team) | Bearer |
| `admin` | every `/api/admin/…` route: `GET /api/admin/teams` (each team with its game at a glance), `DELETE /api/admin/teams/{team_id}`, `DELETE /api/admin/teams/{team_id}/members/{user_id}[?new_leader=<uuid>]`, `POST /api/admin/teams/{team_id}/revive`, `POST /api/admin/teams/{team_id}/cooldowns/reset`, `POST /api/admin/teams/{team_id}/powerups/{kind}`, `GET` and `POST /api/admin/qtes`, `POST /api/admin/qtes/{qte_id}/start` and `/end`, `GET /api/admin/users[?email=]`, `PUT /api/admin/users/{user_id}/role` | Bearer + `ADMIN` in Postgres (`403 not_admin`) |
| `qr` | `POST /api/qr/verify` | Bearer |
| `qr` (staff) | `GET /api/qr/{waypoint_id}` | Bearer + `HELPER`/`ADMIN` in Postgres (`403 not_staff`) |

`/health` is the only route that touches Postgres on every call. `streams` counts every open subscription (all users, all tabs). The rest of its payload is this process's uptime and resource use (goroutines, heap memory, RSS, CPU time, load average), read on demand, not sampled.

Any other path or method:

| Case | Result |
|---|---|
| Unknown path (`GET /api/nope`), with or without a token | `404 not_found`, `no such route` |
| Known path, wrong method (`DELETE /api/teams`, `POST /api/me`) | `405 method_not_allowed`, `method not allowed`, `Allow` lists the methods (e.g. `POST`; `GET, HEAD` for a `GET` route) |
| A path the mux redirects (`/api//me` → `/api/me`) | The mux's `301`, unchanged |

## Rules and cases

### Startup
| Case | Result |
|---|---|
| A required `.env` variable missing, a bad URL or key, or a bad `config.toml` | `config.Load` lists every problem; exit `1` |
| Postgres unreachable | `connect to database: …`; exit `1` |
| A migration fails | `migration NNNN_name: …`; earlier migrations stay applied; exit `1` |
| Another backend holds the instance lock (serving, `migrate` or `seed`) | `another backend instance is already running against this database`; exit `1`, before migrating or seeding |
| An applied migration was edited | `migration NNNN_name: migration was edited after it was applied; …`; nothing new applied; exit `1` |
| `atlas.Load` fails: bad content (`invalid atlas: …`, every case in `internal/atlas/README.md`) or a failed read (`load atlas: …`) | `Build` returns the error; exit `1`, before teams or team state load |
| `bosses.Load` fails: bad boss content (`invalid bosses: …`, e.g. a count other than `game.bosses`; every case in `internal/bosses/README.md`) or a failed read (`load bosses: …`) | `Build` returns the error; exit `1` |
| `teams.Load` or `teamstate.Load` fails (unreachable database, stored team state naming a waypoint, pattern or shape the atlas doesn't have, or a `team_boss` row with more bosses defeated than `game.bosses`) | `Build` returns the error; exit `1` |
| Port in use | `ListenAndServe`'s error; exit `1` |

### `/health`
| Case | Result |
|---|---|
| Postgres answers a ping | `200`, `{"uptime_ms": …, "streams": n, "go_version": …, "goroutines": …, …, "cpu": {…}, "memory": {…}}` |
| Postgres down | `503`, `database unreachable` |

### Event stream
| Case | Result |
|---|---|
| No token, or a bad one | `401` (no stream) |
| Only the `labyrinth_token` cookie, on `/api/events` | Accepted, same as Bearer (`TestStreamCookieAuth`) |
| Only the cookie, on any other route | `401 missing_token` |
| User in no team | `hello`, then `no_team`, then `leaderboard` |
| User in a team | `hello`, then `team`, then `team_state`, then `leaderboard` |
| A change to the user's team or state | One event per change, on every one of the user's streams (see [What publishes what](#what-publishes-what)) |
| Time passes with nobody acting | `: ping` after each idle `live.heartbeat`, `team_state: boss_attacked` / `knocked_out` each time a `short_qr_cooldown` window ends (`powerup_ended`), each time the team's boss attacks, and `team_state: qr_cooldown_ended` each time some of its waypoints open again |
| A rejected or no-op request | Nothing |
| Several tabs | Each tab is its own subscription; each gets every event |
| Client disconnects | `Serve` returns on the request context; subscription removed; `/health` count drops |
| A queued event arrives while **16** (`live.subscriber_buffer`) are already unread | That stream is dropped: the buffered events are written, then the response ends. The browser's `EventSource` reconnects and gets fresh snapshots |
| A write to the client fails, or takes over **10 s** (`live.write_timeout`) | `Serve` returns; subscription removed |
| Server shuts down | `Hub.Close` closes every subscription; each `Serve` returns. `TeamState.Close` stops the team timers (attacks, QR cooldown ends) |

### Body limit
| Case | Result |
|---|---|
| Body ≤ 1,048,576 bytes (`server.max_body_bytes`) | Read normally |
| Body > 1,048,576 bytes on a teams or teamstate write | `400 invalid_body` (`TestSelectAndScanRoutes`) |
| Body > 1,048,576 bytes on `POST /auth/token` | `400 invalid_body` |
| Body still arriving **30 s** (`server.read_timeout`) after the connection was accepted | Read fails; the handler's decode fails (`400`) or the connection closes |

## Concurrency
- **`Build` is single-threaded** and runs before the server listens. Nothing is served until every store is loaded.
- **Requests run concurrently.** All shared state is in the stores (their own locks) and the hub (**64** shards, `live.hub_shards`, each with its own mutex: a `Send` locks one, a `Broadcast` each in turn). `App` itself is read-only after `Build`.
- **Snapshots are read lock-free** from the stores. `team` and `team_state` are read separately, so a change between the two reads can make them disagree briefly; the change's own events follow and correct it.
- **The sweep goroutine** only touches Postgres (`auth_code`), never memory.
- **One process per database** is enforced by the instance lock, which the stores' "memory is the truth" design relies on. Its watcher goroutine checks it every **5 s** (`storage.instance_lock_check`) on the lock's own connection.
- **The sign-in hook** runs in the callback's goroutine and takes the user's team entry lock like any team write.
- **The start hook** runs inside `teams.Start`, holding the team's teams entry lock, and takes the team's teamstate entry lock (`PublishTeam`). So the lock order across the two use cases is teams entry → teamstate entry. teamstate never takes a teams entry lock (it reads teams through `ByID`/`ForUser`, which take only teams' index lock, briefly), so this can't deadlock.
- **The roster hook** runs inside `teams.RemoveMember`, holding the team's teams entry lock, and takes the team's teamstate entry lock (`RosterChanged`): the same order as the start hook. A game write by the removed player either finishes under the teamstate lock before the hook gets it, or re-checks membership under that lock after the removal and gets `404 not_in_team` (`TestAdminRemoveRacesScan`).
- **The admin guard** reads `userprofile` on every admin request (`users.Get`): no memory, so nothing to race.
- **Attack timers** run in their own goroutines (`time.AfterFunc`) and take the team's teamstate entry lock like any write, so an attack never interleaves with a request's write; every write catches up on due attacks first (teamstate README, "Boss attacks and health").

## Failure and recovery
| Case | Result |
|---|---|
| Postgres down while serving | `/health` → `503`. Reads keep working from memory; writes fail and reconcile (see each use case). Streams stay open |
| A sweep fails | Logged as a warning; retried next interval |
| Shutdown takes over **10 s** (`server.shutdown_timeout`) | `Shutdown` returns `context deadline exceeded`; exit `1`; deferred cleanup still runs |
| Process killed without shutdown | The instance lock's session ends with the connection, so Postgres frees it; the next start succeeds. Memory is rebuilt from Postgres |
| The instance lock's connection dies while serving (Postgres restart) | Noticed at the next check (**5 s**, `storage.instance_lock_check`); graceful shutdown; exit `1` with `lost the instance lock`. A supervisor restarts it |
| `go run . migrate` or `seed` while the backend serves | `ErrAnotherInstance`; nothing touched. Stop the backend first |
| Restart | `Build` reloads everything; clients' `EventSource` reconnects and gets fresh snapshots. Tokens stay valid if `JWT_PRIVATE_KEY` is unchanged |

## Limits and how to change them
| Limit | Where | To change |
|---|---|---|
| Body size 1,048,576 bytes | `server.max_body_bytes` (≥ 1) | Edit and restart |
| Header read 10 s | `server.read_header_timeout` | Edit and restart |
| Whole request read 30 s | `server.read_timeout` (≥ `server.read_header_timeout`) | Edit and restart |
| Instance lock check every 5 s | `storage.instance_lock_check` (> 0); also the longest a lost lock goes unnoticed, plus the check's own 5 s timeout | Edit and restart |
| Idle keep-alive 2 m | `server.idle_timeout` | Edit and restart |
| Shutdown wait 10 s | `server.shutdown_timeout`; keep it above `storage.write_timeout` | Edit and restart |
| Heartbeat 25 s | `live.heartbeat`; keep it under any proxy's idle timeout | Edit and restart |
| Slow-client buffer 16 events | `live.subscriber_buffer` | Edit and restart |
| Hub shards 64 | `live.hub_shards` (≥ 1) | Edit and restart |
| Stream write deadline 10 s | `live.write_timeout` (> 0) | Edit and restart |
| Reconnect delay 1–6 s | `live.retry_min` (≥ 1 ms), `live.retry_max` (≥ `retry_min`) | Edit and restart |
| Open streams | The process's file-descriptor limit: one per stream, plus Postgres and listeners. Game day (~10,000 phones) needs well over 10,000 (`ulimit -n`), and any proxy's idle timeout must exceed `live.heartbeat` | Raise the limit in the service config |
| Sweep every 10 m | `auth.login_code_sweep` | Edit and restart |
| Port 3000 | `PORT` in `.env` | Set and restart |

## Not decided yet
- **A lost instance lock is noticed only at the next check**, up to **5 s** (`storage.instance_lock_check`) plus the check's timeout. A second backend starting in that window would run alongside this one briefly.
- **Production routing of `/api/events`** (reverse proxy straight to the backend) waits for the deployment setup.
- **The schemas of future game-wide events**, and slimming `team_state` (it resends `waypoints`, `patterns` and `shapes`, which only grow), wait for the schema pass.

## The test harness
The `*_test.go` files are package `server_test`. They run the real `server.Build` handler over HTTP against `TEST_DATABASE_URL`, with a fake Google. Without `TEST_DATABASE_URL` every test skips.

**⚠ `setup` runs `DROP SCHEMA public CASCADE` on `TEST_DATABASE_URL`.** Point it only at a disposable database.

### `setup(t, allowedDomains...)` (`tests/auth_test.go`)
1. Skip if `TEST_DATABASE_URL` is unset.
2. Wipe: `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`. This happens **per test** (every `setup` call), not once per run.
3. `postgres.Connect`, then `Migrate` **twice** (the second run must be a no-op).
4. `env.identity`: the claims the fake Google puts in its next `id_token`: `sub google-sub-1`, `email player@snu.edu.in`, `email_verified true`, `name Player One`, `picture https://example.com/p.png`. Tests edit it to simulate other users.
5. `fakeGoogle()`: an `httptest.Server` with an RSA-2048 key (`kid k1`):
   - `/jwks`: the public key
   - `/token`: needs `code=good-code` and a non-empty `code_verifier` (PKCE), else `400 invalid_grant`; returns an RS256 `id_token` with `iss` = the fake's URL, `aud` = `test-client.apps.googleusercontent.com`, 1 h expiry, plus `env.identity`
6. The backend: an `httptest.Server` whose handler loads from an `atomic.Pointer[http.Handler]`, so `restart` can swap it while keeping the URL.
7. A `config.Config` built directly, not by `config.Load`: a fresh Ed25519 key, `Settings` from `configtest.Settings(t)` (the repository's `config.toml`), the fake client ID and secret, `PublicURL` = the test server, `FrontendURL` = `http://frontend.test`, and `allowedDomains`.
8. `auth.GoogleEndpoints` pointing at the fake (`/auth`, `/token`, `/jwks`, issuer = its URL).
9. A `user.Users` for planting users, and an HTTP client that doesn't follow redirects.
10. `restart()`.

### `restart()`
Calls `server.Build` again on the same database and config, and swaps the handler. This is what a backend restart does: memory is rebuilt from Postgres, and tokens stay valid (same key). Used after seeding, because content is read at startup. It then closes the previous app's hub and its team state's attack timers (`TeamState.Close`), as a real restart would, so streams opened before it end and no old timer runs.

### Request helpers
| Helper | File | What it does |
|---|---|---|
| `do(req)` | `tests/auth_test.go` | Sends without following redirects; closes the body at cleanup |
| `get(path, cookies...)` | `tests/auth_test.go` | `GET` with cookies |
| `login(next)` | `tests/auth_test.go` | `GET /auth/google/login`; checks `302`, PKCE `S256`, `client_id`, `redirect_uri`, and one `HttpOnly` state cookie; returns `state` and the cookie |
| `callback(query, cookie)` | `tests/auth_test.go` | `GET /auth/google/callback`; checks `302` to `http://frontend.test/auth/…`; returns the redirect URL |
| `redeem(code)` | `tests/auth_test.go` | `POST /auth/token {"code"}`; returns status and decoded body |
| `me(token)` | `tests/auth_test.go` | `GET /api/me` |
| `signIn()` | `tests/auth_test.go` | Runs the whole Google flow for `env.identity` (login, callback, redeem); returns the access token |
| `newPlayer(name)` | `tests/teams_test.go` | Signs in `name@snu.edu.in` straight through `user.SignInWithGoogle` and `Tokens.IssueAccess`, skipping OAuth; returns `player{id, token}` |
| `call(method, path, p, body)` | `tests/teams_test.go` | JSON-encodes `body` (none if `nil`), sends with `p`'s Bearer token (none if empty), decodes the envelope into `apiResult{status, code, payload}` |
| `expect(t, what, got, status, code)` | `tests/teams_test.go` | Fails unless both status and code match (`""` for success) |
| `members(r)` | `tests/teams_test.go` | The `members` array of a team payload |
| `fullTeam(prefix)` | `tests/teams_test.go` | `teams.size` players; `[0]` creates, the rest join |
| `startedTeam(prefix)` | `tests/teamstate_test.go` | `fullTeam`, everyone but the leader ready, leader starts |
| `newAdmin(name)` | `tests/admin_test.go` | `newPlayer`, then `UPDATE userprofile SET role = 'ADMIN'`: the token still says `PLAYER` |
| `callList(method, path, p)` | `tests/admin_test.go` | sends a request as `p` and returns the status and the payload as an array (`call` decodes objects only): the admin teams list, user search |
| `castOver(leader, shape, ids)` | `tests/admin_test.go` | selects `shape` as the leader and scans `ids` with fresh QR tokens, failing unless the last scan completes the pattern |
| `setRole(p, role)` | `tests/admin_test.go` | Sets `p`'s role in Postgres |
| `removePath(teamID, userID, newLeader)` | `tests/admin_test.go` | `/api/admin/teams/{team_id}/members/{user_id}`, plus `?new_leader=` if given |
| `teamOfSize(prefix, n)` | `tests/simulation_test.go` | `n` players form a team over HTTP (`[0]` creates, the rest join and ready), and the leader starts it |
| `newSimClient(p, up, down)` | `tests/simulation_test.go` | A simulated phone: `p`'s stream, each `team_state` delivered `down` after it arrived (in order), `no_team` noted (`noTeam`). `apply(state)` keeps a state only if it's newer by `version` (epoch differs or seq higher, as the frontend's `isNewer`), so a response and its event can arrive in either order. `scan(w)` and `call(method, path, body)` send after `up`, return after `down`, and apply any team state in the response |

### `openStream(p)` (`tests/events_test.go`)
- Opens `GET /api/events` with `p`'s Bearer token on a cancellable context (cancelled at cleanup), through `openStreamWith(auth)`, which lets `auth` set the credentials (a cookie, for instance). Fails unless `200` and `Content-Type: text/event-stream`.
- A goroutine scans lines: `event: X` sets the name; `data: {...}` is parsed as the envelope, giving `sseEvent{name, typ, data}` with `data` the envelope's `message` (so assertions read the payload directly), and routed by name: `hello` to `hellos`, `team_state` to `states`, anything else to `events`. Comment lines (`: ping`) and `retry:` are ignored. The channels hold 64 and close when the body ends.
- Events land on one channel per kind: `hellos` (`hello`), `states` (`team_state`), `boards` (the game-wide `leaderboard`) and `events` (everything else: `team`, `no_team`), so a test waiting for one kind isn't handed another.
- `stream.next(t, what)`: the next `team` or `no_team` event, failing after 3 s or if the stream closed. `stream.nextState(t, what)` likewise for the next `team_state`. Leaderboard events are read straight from `s.boards`.
- `stream.seen()`: every event received so far as `name:type` (`team_state:spell_cast`), across all channels, in arrival order. `waitSeen(t, what, s, want...)` waits up to 3 s for it to equal `want`.
- `expectTeam(t, what, ev, n)`: fails unless `ev` is `team` with `n` members; returns its data. `expectNoTeam` likewise for `no_team`. `memberReady(team, id)` reads a member's `is_ready`.
- `stream.cancel()` disconnects, which is how cleanup is tested.

## API docs (OpenAPI)
- **`openapi.yaml`** (in this package) is the hand-written OpenAPI 3.1 description of every route: request bodies, every success payload as a schema, and for each error response the `code`s it can carry, listed in the response's `description` as ``Codes: `a`, `b` ``. The SSE stream is documented in prose (OpenAPI can't describe event streams), with a sample.
- **`docs.go`** embeds it (`//go:embed`) and serves:
  - `openAPI`: `GET /openapi.yaml` with `Content-Type: application/yaml`
  - `docs`: `GET /docs`, a Swagger UI page loading `swagger-ui-dist@5.17.14` (`swaggerUIVersion`) from jsDelivr. The page needs internet access; the spec doesn't.
  - Both are public and need no token. Use **Authorize** in Swagger UI with a token from `POST /auth/token` to try routes.
- **Kept true by `tests/openapi_test.go`**, which reads the spec and the Go source:

  | Test | Fails when |
  |---|---|
  | `TestOpenAPIMatchesRoutes` | a route registered with `mux.Handle`/`HandleFunc` isn't in the spec, or the spec documents a route nothing registers (`/openapi.yaml` and `/docs` are exempt) |
  | `TestOpenAPIErrorCodes` | a code sent by `httpx.ErrorCode` or an `errorCodes` row isn't listed under the status it's sent with, or the spec lists a code (or a code/status pair) the code never sends. `not_found` and `method_not_allowed` only need to appear once (they're described in `info`). A new `http.Status…` used with a code fails until it's added to `statusByName` |
  | `TestOpenAPIRefsResolve` | a `$ref` points at a component that doesn't exist |
  | `TestDocsServed` | either route isn't served, or has the wrong content type |
- **Not checked by tests:** that schemas match the JSON field by field, and that the spec passes a full OpenAPI validator. Keep schemas in step with the Go types by hand.

## Tests
| Test | File | Covers |
|---|---|---|
| `TestFullLoginFlow` | `tests/auth_test.go` | login → Google → callback → redeem → `/api/me`; code reuse is `400`; a role changed in SQL shows on `/api/me` without a new token; signing in again keeps the user ID and role and updates the name |
| `TestCallbackFailures` | `tests/auth_test.go` | `ALLOWED_EMAIL_DOMAINS=snu.edu.in`. Subtests, each landing on `/auth/error?reason=`: `state_missing`, `state_mismatch`, `state_invalid` (tampered cookie), `google_access_denied`, `google_exchange_failed`, `email_unverified`, `email_domain_not_allowed`; and `next=//evil.com` becomes `/` |
| `TestTokenAndAuthErrors` | `tests/auth_test.go` | expired and never-issued codes are `400 invalid_code`, an empty code `400 invalid_body`; `DeleteExpired` removes exactly the expired one; no token is `401 missing_token`, a garbage token `401 invalid_token`; `/health` is `200`; `POST /api/me` is `405` |
| `TestHealthStats` | `tests/health_test.go` | `GET /health` without a token is `200`, `success` true, `streams` 0, a non-empty `go_version`, ≥1 `goroutines` and `num_cpu`, non-zero `memory.alloc`; on Linux also non-zero `memory.rss` and non-negative `cpu.process_seconds` |
| `TestSignInRefreshesTeamProfile` | `tests/auth_test.go` | a team creator signs in again with a new name and no picture: `GET /api/teams/me` shows the new name and the old avatar, with no restart |
| `TestEmailInUse` | `tests/auth_test.go` | another account already has `player@snu.edu.in`: the callback lands on `reason=email_in_use` |
| `TestStateAndAccessTokensNotInterchangeable` | `tests/auth_test.go` | the OAuth state cookie used as a bearer token is `401 invalid_token`; an access token used as the state cookie lands on `reason=state_invalid` |
| `TestTeamLifecycle` | `tests/teams_test.go` | every teams rule and code on the way from create to start: `401`, `not_in_team`, name trimming, `already_in_team`, `team_full`, `team_not_found`, `invalid_team_id`, `leader_cannot_ready`, `invalid_body`, `not_leader`, `not_all_ready`, rejoin resets ready, `team_locked` for start/leave/ready/join in game; members carry no email |
| `TestLeaderLeaving` | `tests/teams_test.go` | leadership passes to the longest-standing member; the new leader can't ready; the last one out deletes the team |
| `TestTeamNameValidation` | `tests/teams_test.go` | empty, spaces, `teams.name_max_length` + 1 characters (33) and a newline are `400`; `teams.name_max_length` × `é` and punctuation are `201` |
| `TestConcurrentJoinsRespectTeamSize` | `tests/teams_test.go` | 2 × (`teams.size` − 1) = 10 concurrent joins into a team of 1: exactly `teams.size − 1` succeed, the rest `team_full` |
| `TestConcurrentStartAndLeave` | `tests/teams_test.go` | 10 rounds of start racing a leave: either started with `teams.size` (6) and the leave `team_locked`, or the leave won and the start then started a team of `teams.size` − 1 (any size may start) |
| `TestStartWithAnySize` | `tests/teams_test.go` | a leader alone starts (`IN_GAME`, 1 member); a team of 3 is `409 not_all_ready` until both non-leaders are ready, then starts. Needs `teams.size` ≥ 3, else the second half skips |
| `TestAdminGuard` | `tests/admin_test.go` | admin routes: no token `401 missing_token`; a player `403 not_admin` (list and remove); promoted by SQL with a `PLAYER` token is let in; demoted is refused on the next request |
| `TestAdminRemoveFromLobby` | `tests/admin_test.go` | `invalid_team_id`, `invalid_user_id` (path and `new_leader`), `team_not_found`, `not_a_member`, `new_leader_required`, `invalid_new_leader` (self, outsider, non-leader with one); a removal sends `no_team` to the removed and `team` to a member; removing the leader with a new leader; `GET /api/admin/teams`; `last_member` with and without `new_leader`; the removed player creates a team |
| `TestAdminRemoveFromStartedTeam` | `tests/admin_test.go` | seed + `restart`; removing a started team's member: `no_team` and no more `team_state` for them; their scan, select and state read `404 not_in_team`; the rest complete a quadrilateral using the removed player's earlier scan; the removed player joins a lobby team; after `restart` the roster is still smaller and the removed player's scan gets `not_in_game` (they're in a lobby team) |
| `TestAdminRemoveRacesScan` | `tests/admin_test.go` | each non-leader's scan racing their removal: removal `200`; the scan `200` with a one-scan stack or `404 not_in_team` with an empty one, never accepted after the removal |
| `TestAdminTeamsOverview` | `tests/admin_test.go` | seed + `restart`; `GET /api/admin/teams`: a lobby team's `game` is null; a started team's has `health` = `game.health`, `cast_count` 1 and `cooldown_class` `class_4` after the quadrilateral, `knocked_out` false and a `boss` |
| `TestAdminDeleteTeam` | `tests/admin_test.go` | `not_admin`, `invalid_team_id`; deleting a started team: a member's stream gets `no_team: team_deleted`, their state is `404 not_in_team`, the list is empty, a member creates a team at once, deleting again is `404 team_not_found` |
| `TestAdminResetCooldownsOverHTTP` | `tests/admin_test.go` | the quadrilateral cools `class_4` and its waypoints (`waypoint_on_cooldown`); `not_admin`, `invalid_team_id`, `team_not_found`, `not_in_game` (a lobby team); the reset empties `cooldown_class` and `qr_cooldowns`, a member's stream gets `team_state: cooldowns_reset`, and the waypoint scans again |
| `TestAdminUsersAndRoles` | `tests/admin_test.go` | `GET /api/admin/users?email=` finds a user case insensitively; `PUT …/role`: `not_admin` (a player), `cannot_change_own_role`, `invalid_role`, `invalid_body`, `invalid_user_id`, `user_not_found`; promoting a player lets them reach an admin route at once (token still `PLAYER`) |
| `TestLeaderboardAndFinish` | `tests/admin_test.go` | every boss at 1 HP, no QR cooldown: one team casts through every boss (quadrilateral and triangle in turn) and becomes `FINISHED` (`team: finished`), its select `409 not_in_game`; `GET /api/leaderboard` puts it first and `finished`, with the other team as `me` at rank 2; a stream opens with `leaderboard:snapshot`; `Leaderboard.Refresh` broadcasts `leaderboard: updated` |
| `TestUnroutedRequestsUseEnvelope` | `tests/teams_test.go` | `GET /api/nope` with and without a token → `404 not_found`; `DELETE /api/teams` → `405 method_not_allowed` |
| `TestSelectAndScanRoutes` | `tests/teamstate_test.go` | seed + `restart`; `not_in_game`, `not_in_team`, `invalid_body` (wrong type, empty, over `server.max_body_bytes`), a tampered token `400 invalid_qr` and a stale one `409 qr_expired` (scans submit `{"qr"}` tokens minted with the test secret), `unknown_shape`, `unknown_waypoint`, `no_shape_selected`; a non-leader's select and clear `403 not_leader`; the leader's select; a quadrilateral with a rejected retry and rescan (`waypoint_already_scanned`), one member scanning a second waypoint (`200`), `pattern_completed`; state after; reselect; clear; a wrong scan (waypoint `1012`, inserted in no pattern) accepted as `wrong_scan` with `stack[0].wrong` and `poisoned` `true`, a teammate's next scan `409 selection_poisoned`, and a reselect making scans work again; after the quadrilateral's cast, reselecting it is `409 shape_on_cooldown` |
| `TestCastLogRoute` | `tests/teamstate_test.go` | seed + `restart`; `GET /api/teams/me/casts` is `404 not_in_team` without a team, `[]` before any cast; after the quadrilateral, a triangle and the quadrilateral again (the cooldown forbids two quadrilaterals in a row), three casts newest first with their `class` and the repeat `already_completed` |
| `TestSpellCooldownOverHTTP` | `tests/teamstate_test.go` | seed + `restart`; two started teams: A's quadrilateral sets `cooldown_class` `class_4` and `cooldown_shapes` (`quadrilateral`) in the scan response and in the stream's cast event; A reselecting the quadrilateral is `409 shape_on_cooldown`, publishing nothing; B still selects the quadrilateral (per team); after `restart` A's cooldown is the same; A's triangle moves it to `class_3`, and the quadrilateral casts again |
| `TestQRCooldownOverHTTP` | `tests/teamstate_test.go` | seed + `restart`, `config.toml`'s `game.qr_cooldown`; two started teams: A's quadrilateral puts 2001, 2002, 2007, 2008 in `qr_cooldowns`, each until `cast_at` + `game.qr_cooldown`, in the scan response and the stream's cast event; with the triangle selected, A scanning 2001 is `409 waypoint_on_cooldown` with no event, 2003 is `200`; B scans 2001 `200` (the cooldown is per team); after a `restart`, A's `qr_cooldowns` are the same and 2002 is still `409 waypoint_on_cooldown`. `TestSelectAndScanRoutes`, `TestCastLogRoute` and `TestSpellCooldownOverHTTP` set `game.qr_cooldown` to 0: their casts reuse waypoints |
| `TestQTEsOverHTTP` | `tests/qte_test.go` | a quiz through the admin and player routes: every create code (`not_admin`, `invalid_body`, `invalid_kind`, `invalid_powerup`, `invalid_question`, `invalid_options`, `invalid_correct_option`, `invalid_window` both ends); a draft (`GET /api/qte` null, `unknown_qte` to answer, `qte_not_started` to end), `invalid_qte_id` to start and to end, `unknown_qte`; the start (`started` on a member's and a teamless stream, `correct_option` null; admins see it), `qte_already_run`, `qte_open` for a create and a start; answers (`invalid_qte_id`, `invalid_body`, `invalid_option`, `not_in_team`, a member right with `qte_answered`, the leader's resend `qte_already_answered`); the admin list (newest first, `stats`); an early end (`closed` with the reveal on both streams, then `GET /api/qte` null, `qte_closed` to answer and to end again); a 1 s quiz closed by its timer |
| `TestPowerupsOverHTTP` | `tests/admin_test.go` | seed + `restart`; the admin grant (`not_admin`, `invalid_team_id`, `team_not_found`, `unknown_powerup`, `not_in_game`; granted with `powerup_granted` on a member's stream; a second wasted) and the leader's route (`unknown_powerup`, `powerup_not_held`, `not_leader`, `full_health`, `not_in_team`; a multiplier with `powerup_activated`, `powerup_active` while armed; an instant cast refusing a bad body, `unknown_waypoint`, `unknown_pattern`, then casting the quadrilateral boosted, a resend `powerup_not_held`; a short window's until); the admin overview shows `powerups` |
| `TestGameShapes` | `tests/teamstate_test.go` | seed + `restart`; `GET /api/game/shapes` without a token is `401 missing_token`; with one (a player in no team): 5 shapes, `quadrilateral` with 210 patterns and `points` `[4]`, every shape's damage range valid, `max_pattern_points` equal to `config.toml`'s, and no `scan_window_ms` |
| `TestAdminRevive` | `tests/admin_test.go` | seed + `restart`; a started team has `health` = `game.health`, `knocked_out` false and a `next_attack_at`; a `team_boss` phase with 0 health, applied by a `restart`, knocks it out (`health` 0, `next_attack_at` null); select and scan `409 knocked_out`; the revive route: a player `403 not_admin`, `000000` `404 team_not_found`, a lobby team `409 not_in_game`; an admin's revive `200` with full health and `attacks` 0, a member's stream gets `team_state:revived`, and selecting works again |
| `TestRateLimitOverHTTP` | `tests/admin_test.go` | `ratelimit` at 3 requests, one more a minute: a user's 4th request is `429 rate_limited`, and so is opening `/api/events` (with `Retry-After`); another user isn't affected; an admin route is limited too; `/health` never is |
| `TestGameBosses` | `tests/teamstate_test.go` | `GET /api/game/bosses` without a token is `401 missing_token`; `[]` before content; after seed + `restart`, `game.bosses` bosses with serials 1… in order, each with name, asset and HP > 0; a started team's state shows `boss` serial 1 at 0, `bosses_defeated` 0, `damage_multiplier` 1; after a quadrilateral over HTTP, `last_cast` hit boss 1 with `dealt` = `damage`, and whether it defeated it follows boss 1's seeded HP (25: it does, so `boss` is serial 2 at 0 and `bosses_defeated` 1; with more HP, `boss.damage` = `dealt`) |
| `TestSimTeamCompletesUnderLatency` | `tests/simulation_test.go` | four simulated players scan a quadrilateral at once, each with its own delays up to 150 ms each way: exactly one `pattern_completed`; every client ends with an empty stack and the quadrilateral completed |
| `TestSimRaceSameWaypoint` | `tests/simulation_test.go` | two players scan the same waypoint at once: one `200`, one `409 waypoint_already_scanned`; every client converges on a one-scan stack |
| `TestSimWrongScanReachesEveryone` | `tests/simulation_test.go` | after `2001`, one player scans `2099` (a test waypoint in no pattern: every 2026 checkpoint would fit) while a teammate scans `2002`: the first gets `wrong_scan`, the other either `200` (it landed first) or `409 selection_poisoned`; every phone sees `poisoned`; the leader's reselect clears it on every phone |
| `TestSim100Teams` | `tests/simulation_test.go` | **100 teams** of every size from 1 to `teams.size` (team *i* has 1 + (*i* / 8) mod 6 players) at once, ~350 phones each on its own delays (0–30 ms each way) and stream; 6 rounds per team by one of 8 strategies: `steady` (members take turns), `solo` (one member scans everything), `sloppy` (a wrong scan of `2099`, added in no pattern, then `409 selection_poisoned`, then a reselect and cast), `racer` (two members scan the same point at once: one `200`, one `409 waypoint_already_scanned`), `retrier` (every scan resent: `409 waypoint_already_scanned`, `409 no_shape_selected` after the completing one), `quitter` (gives up every other attempt: `DELETE …/selection`), `shrinking` (an admin removes a member over HTTP half way; their scan is `404 not_in_team`), `powered` (each round an admin grants an instant cast and a multiplier over HTTP; the leader arms the multiplier and instant-casts, boosted; a resend is `409 powerup_not_held`, a member `403 not_leader`). Then a quiz over HTTP: an admin sends it out, four teams in five answer (two members at once where two are active: one `200`, one `409 qte_already_answered`), the admin's `stats` count the answers, right ones and grants, the admin ends it, every phone's stream must hold `started` without the answer and `closed` with it, and the fifth team's late answer is `409 qte_closed`. Bosses never attack (`game.boss_attack.interval` set to 10 years in the test's `e.cfg`), and the QR cooldown is off (`game.qr_cooldown` 0: back-to-back rounds reuse the seed's 10 waypoints; `TestQRCooldownOverHTTP` and the teamstate simulation cover it), and the per-user rate limit is raised (`[ratelimit]` to 1000: one-player teams on fast phones and the admin's grants would trip it; `TestRateLimitOverHTTP` covers it). Every select and clear is made by the team's leader (player 0, never the one removed). Every request's status and code is checked; at the end each team's `cast_count` equals its casts, every phone's newest-by-version state has the team's `version` and `cast_count`, and every removed player got `no_team`. ~7 s with the race detector. Each team picks its next pattern of another size than its last cast (the spell cooldown) |
| `TestTeamEventStream` | `tests/events_test.go` | initial `team` / `no_team`; join, ready, leader leaving (new leader), deletion (`no_team`) arrive live; the stream follows the user into a new team; other users' streams get nothing |
| `TestEventStreamAuthAndCleanup` | `tests/events_test.go` | no token is `401`; `/health` counts 1 open stream, then 0 within 2 s of disconnecting |
| `TestStreamStartsWithHello` | `tests/events_test.go` | a stream opens with `hello:connected`, its `server_time` within a second of now, then `no_team:snapshot`, then `leaderboard:snapshot` (waited for before the sequence is read) |
| `TestEventTypes` | `tests/events_test.go` | every publish site over HTTP gets its envelope type, per stream and in order: created, member_joined, member_left / no_team:left, ready_changed, started + game_started, shape_selected (by the leader), scan_added ×3, boss_defeated (the seed's boss 1 has 25 HP, so the quadrilateral's cast defeats it), wrong_scan, selection_cleared (by the leader); an admin's removals (member_removed / leader_changed / no_team:removed); a lobby leader leaving (leader_changed / left) and the last member (team_deleted). Also: team_state's `version.seq` up by one per change in one epoch, and `cast_count` 0 until the cast, then 1 |
| `TestStreamCookieAuth` | `tests/events_test.go` | `/api/events` accepts the `labyrinth_token` cookie; `GET /api/teams/me` and `POST /api/teams` with only the cookie are `401`, as is `/api/events` with a bad cookie; the old stream path `/api/teams/me/events` is `404` |
| `TestCastPublishesDamageOverEventStream` | `tests/events_test.go` | seed + `restart`; a started team casts the triangle `2001 2003 2004`: the stream's `team_state` carries `last_cast.damage`, positive and equal to that pattern's stored `pattern.damage` |
| `TestQRGuard` | `tests/qr_test.go` | seed + `restart`; `GET /api/qr/{waypoint_id}`: no token `401 missing_token`; a player `403 not_staff` (role checked before the ID); as `HELPER` (token still `PLAYER`) `400 invalid_waypoint_id`, `404 unknown_waypoint`, and a mint with the key; `ADMIN` mints; demoted back to `PLAYER` is `403 not_staff` |
| `TestQRVerify` | `tests/qr_test.go` | `POST /api/qr/verify`: a fresh token gives its waypoint, an old one `409 qr_expired`, a tampered signature `400 invalid_qr`, an empty body `400 invalid_body` |
| `TestStreamIncludesTeamState` | `tests/events_test.go` | `team_state` follows `team` on connect; `GET /api/teams/me/state` works, and is `404 not_in_team` without a team |
| `TestStartPublishesTeamState` | `tests/events_test.go` | a full, ready team with every member's stream open: when the leader starts, each stream gets the `IN_GAME` `team`, then an empty `team_state`, in that order (events seen: `hello:connected`, `team:snapshot`, `team_state:snapshot`, `team:started`, `team_state:game_started`), without reconnecting |

Team sizes and name lengths come from `config.toml` through `e.cfg.Settings`, never repeated as numbers.

**The simulation tests** (`tests/simulation_test.go`, up to 100 teams at once in `TestSim100Teams`) model each player as a phone: its own request and response delays, and its own event stream (each event delivered after the response delay, in order). There's no time limit on an attempt, and they finish long before the first boss attack (**10 min**, `game.boss_attack.interval`), so they test concurrency and convergence only: scans racing over uneven networks, and every phone ending on the same `team_state`. They use real time (`time.Sleep`), not a fake clock, because the delays are network delays over real HTTP.

**Boss attacks over real time** aren't tested here (they'd need minutes of real time; `TestAdminRevive` knocks a team out through a stored phase instead): the timer and catch-up are tested with a fake clock in `internal/teamstate/tests/attack_test.go`. `env.restart` closes the old app's `TeamState` (and its `Hub`), so a replaced app's timers never run.

**Not covered here:** `/health` returning `503`, the heartbeat (tests wait at most 3 s; covered in `platform/live`), a slow stream being dropped over HTTP (covered in `platform/live`), `Hub.Close` on shutdown, the `Allow` header's value, and `main.go` itself (subcommands, lock ordering, lost-lock shutdown, sweep loop, timeouts).
