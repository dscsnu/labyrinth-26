# server

Wires the use cases together, serves the routes that span several of them (`/healthz`, the live event stream, and the API docs at `/openapi.yaml` and `/docs`), and holds the end-to-end tests. It has no rules of its own. `main.go` (in `backend/`) runs it: startup, the background sweep, and shutdown are documented here too.

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
| **`App`** | What `Build` returns: the HTTP handler plus the pieces `main` and the tests need (`Hub`, `Atlas`, `Tokens`, `LoginCodes`, `Teams`, `TeamState`). `Atlas` is the `*atlas.Atlas` (static content); nothing reads `App.Atlas` today (`Build` passes the atlas to teamstate directly), it's there for `main`, tests and future use cases. Unexported: the `*postgres.DB` (for `/healthz`) and the heartbeat. |
| **Build order** | Each use case is constructed after everything it reads. `atlas` is static content every game use case reads, so `atlas.Load` runs first. `teamstate` reads `atlas` and `teams`, and auth's sign-in hook calls `teams`, so `teams.Load` runs before both. |
| **Authed route** | A route wrapped in auth's `RequireAuth` (passed to each use case as `authAPI.Authed`). It needs `Authorization: Bearer <jwt>` and puts the caller on the request via `platform/identity`. |
| **Stream** | One open `GET /api/teams/me/events`: one `live.Subscription` in the hub, keyed by the caller's user ID. |
| **Initial snapshots** | The events a stream sends before anything else: `team` or `no_team`, then `team_state` if the caller is in a team. |
| **Instance lock** | A Postgres advisory lock (key `7272027`) the process holds from just after connecting until it exits, checked every **5 s** (`storage.instance_lock_check`). A second backend, `migrate` or `seed` on the same database refuses to run. |
| **Unrouted request** | One no route pattern matches: an unknown path, or a known path with the wrong method. `envelopeUnrouted` answers it in the JSON envelope. |
| **Sign-in hook** | The `onSignIn` function `Build` gives auth: after every sign-in it calls `teams.Store.RefreshProfile`, so a renamed user shows the new name in their team at once. |
| **Start hook** | The `teams.Rules.OnStart` function `Build` gives teams: after a team starts (and its `team` event is published) it calls `teamstate.Store.PublishTeam`, so every member's open stream gets the team's (empty) `team_state` at once. teams can't import teamstate (AGENTS.md §4), hence the callback. |

## Startup and shutdown
`main.go`'s `run` does these steps in order. Any error returned is logged as `fatal` and the process exits with status `1`.

| # | Step | Detail |
|---|---|---|
| 1 | Logger | `slog` text handler on stdout, also set as `slog.SetDefault` (for `httpx`, which has no logger of its own). |
| 2 | `keygen` subcommand | Runs **before** `.env` and config: prints a fresh `JWT_PRIVATE_KEY` / `JWT_PUBLIC_KEY` pair and exits. |
| 3 | Load `.env` | `godotenv.Load()` reads `./.env` if it exists. A missing file is fine (deployments set real environment variables). Variables already set are not overridden. Any other read error is fatal. |
| 4 | `pubkey` subcommand | Parses `JWT_PRIVATE_KEY`, prints its `JWT_PUBLIC_KEY`, exits. |
| 5 | `config.Load()` | Reads `.env` variables and `config.toml`; every problem is reported at once (see `internal/platform/README.md`). |
| 6 | Signal context | `signal.NotifyContext` on `SIGINT` and `SIGTERM`. Everything below uses this `ctx`. |
| 7 | `postgres.Connect` | Opens the pool and pings. Failure: `connect to database: …`. |
| 8 | `db.AcquireInstanceLock(ctx, storage.instance_lock_check)` | Takes the instance lock, for serving **and** for `migrate` and `seed`. Another backend holding it: `ErrAnotherInstance`, exit `1`, before anything is migrated or seeded. Deferred `lock.Release()`. |
| 9 | `db.Migrate` | Applies new migrations under the migration advisory lock (key `7272026`). An edited applied migration: `ErrMigrationEdited`, exit `1`. |
| 10 | `migrate` subcommand | Exits here. |
| 11 | `seed` subcommand | `db.SeedDev2025`, logs `seeded 2025 placeholder content`, exits. Content is read at startup, which is why seeding needs the lock. |
| 12 | `server.Build` | Loads memory from Postgres and builds the routes (see [`server.go`](#servergo)). |
| 13 | Login code sweep | `go cleanupLoginCodes(...)`: every **10 m** (`auth.login_code_sweep`) deletes expired, unredeemed login codes. The first sweep is one interval after startup, not at startup. |
| 14 | `http.Server` | `Addr ":" + PORT` (default `3000`), `ReadHeaderTimeout` **10 s** (`server.read_header_timeout`), `ReadTimeout` **30 s** (`server.read_timeout`), `IdleTimeout` **2 m** (`server.idle_timeout`). No `WriteTimeout`: event streams are long-lived. `live.Serve` clears the read deadline once a stream starts. |
| 15 | Shutdown hook | `srv.RegisterOnShutdown(app.Hub.Close)`. |
| 16 | Serve | `ListenAndServe` in a goroutine, logging `listening addr=:3000`. |
| 17 | Wait | For a signal, for `lock.Lost()` to close, or for `ListenAndServe` to fail (e.g. port in use), whose error is returned at once. |

**Shutdown**, on `SIGINT`, `SIGTERM` or a lost instance lock:
1. On a lost lock, logs `instance lock lost; shutting down` (error). Then logs `shutting down`. In both cases `ctx` is cancelled (a lost lock calls `stop()`), so the sweep goroutine returns.
2. `srv.Shutdown` with a **10 s** (`server.shutdown_timeout`) deadline. It closes the listener, runs `Hub.Close` (which closes every subscription channel, so every `live.Serve` returns), then waits for in-flight requests.
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
  1. `live.NewHub(live.subscriber_buffer)`: the shared hub. Every use case that publishes gets this one.
  2. `user.New(db)`: Postgres-only users.
  3. `auth.NewTokens(cfg.JWTKey, auth.token_ttl)`: the JWT signer and verifier.
  4. `auth.NewLoginCodes(db, auth.login_code_ttl)`.
  5. `auth.NewGoogle(GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET, PUBLIC_URL + "/auth/google/callback", google)`.
  6. `atlas.Load(ctx, db, log, atlas.Rules{MaxPatternPoints: game.max_pattern_points})`: reads and validates the static content (waypoints, shapes, patterns) once. First, because every game use case reads it. An error (`load atlas: …`, `invalid atlas: …`) is returned as is. See `internal/atlas/README.md`.
  7. `teams.Load(ctx, db, hub, log, teams.Rules{…, OnStart})`: reads every team into memory. An error is returned as is. `OnStart` is `func(t) { stateStore.PublishTeam(t.ID) }`, the start hook; `stateStore` is declared before this step and assigned in step 9, which is safe because no team can start before `Build` returns and routes are served.
  8. `auth.NewAPI(cfg, google, tokens, codes, users, onSignIn, log)`, after teams because `onSignIn` is `func(u) { teamStore.RefreshProfile(u.ID, u.Name, u.AvatarURL) }`. It reads `auth.oauth_state_ttl` and `SecureCookies()` from `cfg` itself.
  9. `teamstate.Load(ctx, db, theAtlas, teamStore, hub, log, teamstate.Rules{…})`: reads every team's state, checking it against the atlas. It needs the atlas (through its `Atlas` interface) and `teamStore` (through its `Teams` interface), so it runs after steps 6 and 7.
  10. The `App` (with `Atlas: theAtlas`), then a `http.ServeMux` with, in this order:
     - `GET /healthz` (no auth)
     - `GET /openapi.yaml` and `GET /docs` (no auth; see [API docs](#api-docs-openapi))
     - `authAPI.Routes(mux)`: the three `/auth/…` routes (no auth)
     - `user`, `teams`, `teamstate`: each `NewAPI(…).Routes(mux, authAPI.Authed)` (`atlas` has no routes)
     - `GET /api/teams/me/events`, wrapped in `authAPI.Authed`
  11. `App.Handler = http.MaxBytesHandler(envelopeUnrouted(mux), server.max_body_bytes)`.
- **`envelopeUnrouted(mux)`** wraps the mux. For each request it asks `mux.Handler(r)` for the matching pattern:
  - a pattern matched → `mux.ServeHTTP` as usual
  - no pattern → runs the mux's own fallback handler into a **`statusRecorder`** (records status and headers, discards the plain-text body), then:
    - `404` → `httpx.ErrorCode(404, "not_found", "no such route")`
    - `405` → copies the mux's `Allow` header, then `httpx.ErrorCode(405, "method_not_allowed", "method not allowed")`
    - anything else → handled by the mux as it would have been (not expected: the mux's redirects have a pattern and never reach this branch)
- **`statusRecorder`**: `Header()`, `WriteHeader(status)` and `Write` (counts the bytes, sets `200` if no status yet) for the above.
- **`health`** handles `GET /healthz`: `db.Ping`; on error `503` `database unreachable`; otherwise `200` with `{"streams": Hub.Count()}`.
- **`events`** handles `GET /api/teams/me/events`: see [The event stream](#the-event-stream). It lives here because it carries events from two use cases.

### `*_test.go`
End-to-end tests for every use case, because they need the whole app. See [The test harness](#the-test-harness).

## Where each config.toml value goes
| Key | Value | Passed to |
|---|---|---|
| `teams.size` | 6 | `teams.Rules.Size` |
| `teams.name_max_length` | 32 | `teams.Rules.NameMaxLength` |
| `teams.code_attempts` | 10 | `teams.Rules.CodeAttempts` |
| `game.scan_window` | 1 m | `teamstate.Rules.ScanWindow` |
| `game.scan_grace` | 5 s | `teamstate.Rules.ScanGrace` |
| `game.max_pattern_points` | 6 | `atlas.Rules.MaxPatternPoints` and `teamstate.Rules.MaxPatternPoints` |
| `auth.token_ttl` | 12 h | `auth.NewTokens` |
| `auth.login_code_ttl` | 2 m | `auth.NewLoginCodes` |
| `auth.oauth_state_ttl` | 10 m | read by `auth.API` from `cfg.Settings` (state token TTL and cookie `MaxAge`) |
| `auth.login_code_sweep` | 10 m | `main.cleanupLoginCodes` |
| `live.heartbeat` | 25 s | `App.heartbeat` → `live.Serve` |
| `live.subscriber_buffer` | 16 | `live.NewHub` |
| `server.read_header_timeout` | 10 s | `http.Server.ReadHeaderTimeout` (`main`) |
| `server.read_timeout` | 30 s | `http.Server.ReadTimeout` (`main`); cleared per stream by `live.Serve` |
| `server.idle_timeout` | 2 m | `http.Server.IdleTimeout` (`main`) |
| `server.shutdown_timeout` | 10 s | the `srv.Shutdown` context (`main`) |
| `server.max_body_bytes` | 1,048,576 | `http.MaxBytesHandler` |
| `storage.write_timeout` | 5 s | `teams.Rules.WriteTimeout` and `teamstate.Rules.WriteTimeout` |
| `storage.instance_lock_check` | 5 s | `db.AcquireInstanceLock` (`main`) |

From `.env` (via `config.Config`): `JWT_PRIVATE_KEY` → `auth.NewTokens`; `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `PUBLIC_URL` → `auth.NewGoogle`; `FRONTEND_URL`, `ALLOWED_EMAIL_DOMAINS` → `auth.NewAPI` (via `cfg`); `DATABASE_URL` and `PORT` → `main`.

## The request path
Outermost first:

```
http.Server              ReadHeaderTimeout 10 s, ReadTimeout 30 s, IdleTimeout 2 m
└ http.MaxBytesHandler    every body capped at 1,048,576 bytes (server.max_body_bytes)
  └ envelopeUnrouted      unmatched → 404 not_found, wrong method → 405 method_not_allowed (JSON)
    └ http.ServeMux       Go 1.22 method patterns
      └ RequireAuth       only on authed routes: Bearer JWT → identity on the context
        └ handler         use case api.go, or health / events
```

- **Body limit.** `MaxBytesHandler` replaces `r.Body` with an `http.MaxBytesReader`. Reading past the limit fails with `*http.MaxBytesError`. Every handler reads bodies with `httpx.DecodeJSON`, so an oversized body surfaces as a decode error: `400 invalid_body` in teams, teamstate and `POST /auth/token`. It applies to every route, including `/auth/…` and `/healthz`, but only matters where a body is read.
- **Read timeout.** A client has **30 s** (`server.read_timeout`) from accepting the connection to finish sending a request, body included, so a trickled body can't hold a connection open. Event streams clear it when they start.
- **Routing before auth.** The mux matches first, so an unknown path is `404 not_found` and a wrong method is `405 method_not_allowed` (with `Allow`) even without a token, both in the JSON envelope:
  ```
  GET /api/nope                 → 404 {"success":false,"code":"not_found","message":"no such route"}
  DELETE /api/teams             → 405 Allow: POST
                                      {"success":false,"code":"method_not_allowed","message":"method not allowed"}
  ```
- **Auth.** `RequireAuth` (in `internal/auth`):
  - no header, a scheme other than `Bearer` (case-insensitive), or an empty token → `401 missing_token`, `WWW-Authenticate: Bearer`
  - a token that fails `VerifyAccess` (bad signature, expired, malformed) → `401 invalid_token`, `WWW-Authenticate: Bearer error="invalid_token"`
  - otherwise the handler runs with `identity.Identity{UserID, Email, Name, Role}` from the token's claims
- No other middleware: no CORS, logging, recovery or rate limiting at this layer.

## The event stream
The complete reference for the live stream: what it carries, what triggers each event, and the guarantees a client can rely on. AGENTS.md §9 and §17 hold the rules for publishing.

### Endpoint
| | |
|---|---|
| Backend | `GET /api/teams/me/events`, `Authorization: Bearer <jwt>` (any signed-in user, in a team or not). No token or a bad one: `401 missing_token` / `401 invalid_token`, no stream |
| Browser | `GET /team/events`, same origin, through the SvelteKit proxy `frontend/src/routes/team/events/+server.ts`: `new EventSource("/team/events")` |
| Proxy | Reads the httpOnly session token (`locals.token`; none → `401 not signed in`), calls the backend with it (`backendFetch`) and returns the backend's body unchanged with `Content-Type: text/event-stream`, `Cache-Control: no-store`, `X-Accel-Buffering: no`. A backend `401` stays `401`; any other failure is `502 event stream unavailable`. The browser closing its request aborts the backend request too (`signal: request.signal`) |
| Response | `200`, `Content-Type: text/event-stream`, `Cache-Control: no-store`, `X-Accel-Buffering: no` (so nginx doesn't buffer). The body never ends on its own |

### How `events` serves a stream
1. `userID := identity.UserID(r)`.
2. **`sub := a.Hub.Subscribe(userID)`**, then `defer sub.Close()`.
3. **Initial snapshots**, read from memory after subscribing:
   - `a.Teams.Snapshot(userID)`: `team` with the same JSON as `GET /api/teams/me`, or `no_team` with `{}` if the user is in no team (or the team failed to encode, which is logged).
   - `a.TeamState.Snapshot(userID)`: if the user is in a team, `team_state` with the same JSON as `GET /api/teams/me/state`. Skipped if not in a team (or on an encode error, logged). Sent for a `LOBBY` team too (an empty state).
4. `live.Serve(w, r, sub, initial, live.heartbeat)`: clears the read deadline, writes the initial events, then every event from `sub`, plus `: ping` after **25 s** (`live.heartbeat`) with no event. Returns when the client disconnects, the subscription is dropped or the hub closes.
5. The deferred `sub.Close()` removes the subscription; the `/healthz` count drops.

**The stream follows the user.** Subscriptions are keyed by user ID, not team. Joining, leaving or a new team all arrive on the same stream (`TestTeamEventStream`). Each tab is its own subscription and gets every event.

### Wire format
Each event is exactly `event: <name>\n` + `data: <compact JSON on one line>\n` + `\n`. A heartbeat is the comment `: ping\n\n`, which `EventSource` ignores. No `id:` or `retry:` fields are sent. Each write is flushed at once.

Byte-level example for a user in the lobby team `482913` with two members, from connect to the first ping (`␤` = `\n`):
```
HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-store
X-Accel-Buffering: no

event: team␤
data: {"id":"482913","name":"Scanners","leader_id":"3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b01","status":"LOBBY","created_at":"2026-10-17T11:50:00Z","started_at":null,"members":[{"id":"3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b01","name":"Asha","avatar_url":null,"is_ready":false,"joined_at":"2026-10-17T11:50:00Z"},{"id":"3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b02","name":"Bilal","avatar_url":"https://example.com/b.png","is_ready":true,"joined_at":"2026-10-17T11:51:30Z"}]}␤
␤
event: team_state␤
data: {"team_id":"482913","waypoints":[],"patterns":[],"shapes":[],"selection":null,"last_cast":null,"stack":[]}␤
␤
: ping␤
␤
```

### Events
| Event | Data | Same JSON as | Schema (`openapi.yaml`) |
|---|---|---|---|
| `team` | the user's team | `GET /api/teams/me` | `Team` |
| `no_team` | `{}` | (`GET /api/teams/me` is `404 not_in_team`) | none |
| `team_state` | the user's team's game state, the `View` | `GET /api/teams/me/state` | `TeamState` |

Every event is a **full snapshot**, never a diff: the newest event of each name is the whole truth.

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
  ]
}
```
The leader can't set `is_ready`, and start ignores the leader's flag. Emails are never included.

**`no_team`**: always the data `{}`. The user is in no team: on connect, after leaving, or when their team was deleted.

**`team_state`**, the same team after it cast the square `1001 1002 1007 1008` at 12:00:12 (Divya's scan), selected `square` again at 12:05:00 (Farah), and scanned `1001` (Asha, 12:05:10) and `1002` (Bilal, 12:05:20); limit **65 s** (`game.scan_window` + `game.scan_grace`):
```json
{
  "team_id": "482913",
  "waypoints": ["1001", "1002", "1007", "1008"],
  "patterns": [["1001", "1002", "1007", "1008"]],
  "shapes": ["square"],
  "selection": {"shape": "square", "selected_by": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b06", "selected_at": "2026-10-17T12:05:00Z"},
  "last_cast": {
    "pattern": ["1001", "1002", "1007", "1008"],
    "shape": "square",
    "cast_by": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b04",
    "cast_at": "2026-10-17T12:00:12Z",
    "already_completed": false
  },
  "stack": [
    {"waypoint_id": "1001", "user_id": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b01", "scanned_at": "2026-10-17T12:05:10Z", "expires_at": "2026-10-17T12:06:15Z"},
    {"waypoint_id": "1002", "user_id": "3f2a6c1e-8b4d-4e9a-a1c2-5d6e7f8a9b02", "scanned_at": "2026-10-17T12:05:20Z", "expires_at": "2026-10-17T12:06:25Z"}
  ]
}
```
| Field | Meaning |
|---|---|
| `team_id` | the team |
| `waypoints` | every waypoint the team ever had a scan accepted at, sorted |
| `patterns` | every completed pattern, each as its sorted waypoint IDs |
| `shapes` | every shape with at least one completed pattern, in atlas index order |
| `selection` | the shape being attempted, who selected it and when; `null` if none |
| `last_cast` | the newest cast: its pattern (sorted IDs), shape, who completed it, when, and `already_completed` (the team had cast that pattern before, so progress didn't grow). Kept until the next cast; a new selection or a clear keeps it. `null` if the team never cast (or only before migration `0007`) |
| `stack` | the live scans, bottom (oldest) first. Stale correct scans are left out; wrong scans are included with nothing marking them |
| `stack[].expires_at` | `scanned_at` + **65 s**: when that correct scan goes stale. At that moment the server publishes a new `team_state` without it (below) |

Every field, rule and case: `internal/teamstate/README.md`, "API".

### What publishes what
Every event below goes to **every open stream** of each listed user. "Members" means the team's members after the change. Each is published after the change is applied, while the writer still holds the entry lock.

| Trigger | Event | To |
|---|---|---|
| **Connect** (`GET /api/teams/me/events`) | `team` (or `no_team`), then `team_state` if in a team | that stream only |
| teams `POST /api/teams` (create) | `team` | the creator (its only member) |
| teams `POST /api/teams/join` | `team` | members, the joiner included |
| teams `POST /api/teams/leave`, others remain | `team` (new `leader_id` if the leader left) | the remaining members |
| | `no_team` | the user who left |
| teams `POST /api/teams/leave`, last member | `no_team` | the user who left (the team is deleted) |
| teams `PUT /api/teams/me/ready`, flag changed | `team` | members |
| teams `PUT /api/teams/me/ready`, flag already that value | nothing (no-op) | |
| teams `POST /api/teams/me/start` | `team` (`status` `IN_GAME`, `started_at` set), then `team_state` (the team's state, empty: no selection, stack `[]`, no progress) through the start hook (`OnStart` → `teamstate.PublishTeam`) | members |
| Sign-in (`onSignIn` → `teams.RefreshProfile`), name or avatar changed | `team` | members |
| Sign-in, nothing changed or not in a team | nothing | |
| teams write fails to persist (`reconcile`) | `team` reloaded from Postgres | its members |
| | `no_team` | users no longer in it (all old members if the team is gone) |
| teamstate `PUT /api/teams/me/state/selection` (select, reselect) | `team_state` (stack purged) | members |
| teamstate `DELETE /api/teams/me/state/selection`, something selected | `team_state` (selection `null`, stack `[]`) | members |
| teamstate `DELETE …/selection`, nothing selected | nothing (no-op) | |
| teamstate `POST /api/teams/me/state/scans`, `scan_added` (wrong scans included) | `team_state` | members |
| teamstate `POST …/scans`, `pattern_completed` (a cast) | `team_state` (selection `null`, stack `[]`, new `last_cast`) | members |
| teamstate expiry timer: a correct scan is now stale (`expire`) | `team_state` (stack without it) | members |
| teamstate expiry timer, nothing stale (a write rebuilt the stack first) | nothing | |
| teamstate write fails to persist (`reconcile`) | `team_state` reloaded from Postgres (nothing if the reload fails too) | members |
| Any rejection (`4xx`) or `500` before persisting | nothing | |

The HTTP response to a write carries the same JSON as its event (`ScanResult.state` for a scan), but on a different connection: a client may see either first.

### Ordering
- **One stream is FIFO.** `Hub.Send` puts events on the subscription's one channel, and `Serve` writes them in that order.
- **Per team, in order.** Each use case publishes while holding the team's entry lock (`teams`' entry, or `teamstate`'s entry for writes, `expire` and `reconcile`), so one team's `team` events arrive in the order they were applied, and so do its `team_state` events.
- **No order between `team` and `team_state`,** with one exception. They're published under different locks; a `team` and a `team_state` from changes at the same moment may arrive either way round. Each is still the full truth for its part. **The exception is a start:** teams calls the start hook after publishing the `IN_GAME` `team`, still holding the team's entry lock, so on every member's stream the start's `team` comes before its `team_state` (`TestStartPublishesTeamState`).
- **Expiry is ordered with writes.** `expire` takes the same entry lock as a scan, and re-checks the stack under it, so a stale-scan event never arrives after a later write that already dropped the scan.

**Why subscribe first.** A change published between reading the snapshot and subscribing would be lost. Subscribing first means any change after the snapshot is also queued. The cost: a change that lands between subscribing and reading the snapshot is delivered **twice** (once inside the snapshot, once queued). Events are full snapshots, so a repeat is harmless.

**A brief step back is possible.** Writers apply a change before publishing it. If change X is published (queued) and change Y is applied before the snapshot is read, the client sees Y (snapshot), then X (queued), then Y (queued once published). The final state is always the newest, because each publish happens after subscribing. `team` and `team_state` snapshots are read separately, so they may also disagree briefly on connect; the change's own events follow and correct it.

### Heartbeat
After **25 s** (`live.heartbeat`) with no event, `Serve` writes `: ping\n\n` and flushes, so mobile networks and proxies don't drop the idle connection. Every event resets the timer, so a busy stream gets no pings. The first ping comes 25 s after the initial events. Keep `live.heartbeat` under any proxy's idle timeout.

### Slow clients
Each subscription buffers **16** (`live.subscriber_buffer`) unread events. `Hub.Send` never blocks: when an event arrives while 16 are unread, that subscription is **dropped**. `Serve` writes the 16 buffered events, then the response ends. The client reconnects and starts from fresh snapshots, so it loses nothing. Other streams, the same user's other tabs included, are unaffected.

### Reconnection
- The browser's `EventSource` reconnects by itself after any end of the response (drop, restart, network loss), after its default delay: no `retry:` is sent.
- Nothing is replayed (no `id:`, so no `Last-Event-ID`). The **first events after each connect are full snapshots** (`team`/`no_team`, then `team_state`), which is all a client needs.
- `/team` and `/play` show "reconnecting…" while `EventSource` is between connections (`onerror`), and "live ●" once it's open.

### Shutdown and restart
- On shutdown, `srv.Shutdown` runs `Hub.Close`, which closes every subscription: each `Serve` returns and its response ends. New subscriptions are refused (closed at once).
- After a restart, `Build` reloads memory, and teamstate re-arms every team's expiry timer. A stack that went stale while the backend was down is dropped at once, before anyone connects. Clients reconnect and get snapshots of the reloaded state.

### Never sent
- Anything for a **rejection** (every `4xx`) or a **no-op** (ready unchanged, clear with nothing selected, a sign-in that changes nothing, an expiry timer that finds nothing stale).
- Whether a scan was **wrong**: `team_state` has no such flag; wrong scans look like correct ones.
- **Stale** correct scans: left out of `stack`.
- **Diffs**, event IDs or `retry:` fields.
- Another team's state, or any member's email.
- `GET /api/game/shapes` (`Game`): static while the backend runs; read once per page load.

## API
| Method | Path | Auth | Response |
|---|---|---|---|
| GET | `/healthz` | none | `200 {"success":true,"payload":{"streams":3}}`, or `503 {"success":false,"message":"database unreachable"}` |
| GET | `/api/teams/me/events` | Bearer | `200 text/event-stream` (see above) |
| GET | `/openapi.yaml` | none | `200 application/yaml`: the OpenAPI 3.1 spec |
| GET | `/docs` | none | `200 text/html`: Swagger UI over `/openapi.yaml` |

Routes registered here but owned by use cases (see their READMEs):

| Use case | Routes | Auth |
|---|---|---|
| `auth` | `GET /auth/google/login`, `GET /auth/google/callback`, `POST /auth/token` | none |
| `user` | `GET /api/me` (`401 user_not_found` if the token's user was deleted) | Bearer |
| `teams` | `GET /api/teams/me`, `POST /api/teams`, `POST /api/teams/join`, `POST /api/teams/leave`, `PUT /api/teams/me/ready`, `POST /api/teams/me/start` | Bearer |
| `teamstate` | `GET /api/game/shapes`, `GET /api/teams/me/state`, `PUT` and `DELETE /api/teams/me/state/selection`, `POST /api/teams/me/state/scans` | Bearer |

`/healthz` is the only route that touches Postgres on every call. `streams` counts every open subscription (all users, all tabs).

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
| `teams.Load` or `teamstate.Load` fails (unreachable database, or stored team state naming a waypoint, pattern or shape the atlas doesn't have) | `Build` returns the error; exit `1` |
| Port in use | `ListenAndServe`'s error; exit `1` |

### `/healthz`
| Case | Result |
|---|---|
| Postgres answers a ping | `200`, `{"streams": n}` |
| Postgres down | `503`, `database unreachable` |

### Event stream
| Case | Result |
|---|---|
| No token, or a bad one | `401` (no stream) |
| User in no team | `no_team` only |
| User in a team | `team`, then `team_state` |
| A change to the user's team or state | One event per change, on every one of the user's streams (see [What publishes what](#what-publishes-what)) |
| A correct scan in the team's stack passes **65 s** (`game.scan_window` + `game.scan_grace`) with nobody acting | `team_state` without it, at the first microsecond it's stale (`TestSimStaleScanReachesEveryone`) |
| A rejected or no-op request | Nothing |
| Several tabs | Each tab is its own subscription; each gets every event |
| Client disconnects | `Serve` returns on the request context; subscription removed; `/healthz` count drops |
| A queued event arrives while **16** (`live.subscriber_buffer`) are already unread | That stream is dropped: the buffered events are written, then the response ends. The browser's `EventSource` reconnects and gets fresh snapshots |
| A write to the client fails | `Serve` returns; subscription removed |
| Server shuts down | `Hub.Close` closes every subscription; each `Serve` returns |

### Body limit
| Case | Result |
|---|---|
| Body ≤ 1,048,576 bytes (`server.max_body_bytes`) | Read normally |
| Body > 1,048,576 bytes on a teams or teamstate write | `400 invalid_body` (`TestSelectAndScanRoutes`) |
| Body > 1,048,576 bytes on `POST /auth/token` | `400 invalid_body` |
| Body still arriving **30 s** (`server.read_timeout`) after the connection was accepted | Read fails; the handler's decode fails (`400`) or the connection closes |

## Concurrency
- **`Build` is single-threaded** and runs before the server listens. Nothing is served until every store is loaded.
- **Requests run concurrently.** All shared state is in the stores (their own locks) and the hub (one mutex). `App` itself is read-only after `Build`.
- **Snapshots are read lock-free** from the stores. `team` and `team_state` are read separately, so a change between the two reads can make them disagree briefly; the change's own events follow and correct it.
- **The sweep goroutine** only touches Postgres (`auth_code`), never memory.
- **teamstate's expiry timers** (`time.AfterFunc`, at most one per team) run in their own goroutines and take the team's entry lock like a write; see `internal/teamstate/README.md`, "Concurrency and locking".
- **One process per database** is enforced by the instance lock, which the stores' "memory is the truth" design relies on. Its watcher goroutine checks it every **5 s** (`storage.instance_lock_check`) on the lock's own connection.
- **The sign-in hook** runs in the callback's goroutine and takes the user's team entry lock like any team write.
- **The start hook** runs inside `teams.Start`, holding the team's teams entry lock, and takes the team's teamstate entry lock (`PublishTeam`). So the lock order across the two use cases is teams entry → teamstate entry. teamstate never takes a teams entry lock (it reads teams through `ByID`/`ForUser`, which take only teams' index lock, briefly), so this can't deadlock.

## Failure and recovery
| Case | Result |
|---|---|
| Postgres down while serving | `/healthz` → `503`. Reads keep working from memory; writes fail and reconcile (see each use case). Streams stay open |
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
| Sweep every 10 m | `auth.login_code_sweep` | Edit and restart |
| Port 3000 | `PORT` in `.env` | Set and restart |

## Not decided yet
- **A lost instance lock is noticed only at the next check**, up to **5 s** (`storage.instance_lock_check`) plus the check's timeout. A second backend starting in that window would run alongside this one briefly.
- **No request logging, panic recovery beyond `net/http`'s, or rate limiting.**

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
Calls `server.Build` again on the same database and config, and swaps the handler. This is what a backend restart does: memory is rebuilt from Postgres, and tokens stay valid (same key). Used after seeding, because content is read at startup. It then closes the previous app's hub, as a real restart would, so streams opened before it end.

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

### `openStream(p)` (`tests/events_test.go`)
- Opens `GET /api/teams/me/events` with `p`'s token on a cancellable context (cancelled at cleanup). Fails unless `200` and `Content-Type: text/event-stream`.
- A goroutine scans lines: `event: X` sets the name; `data: {...}` is parsed as JSON and sent to `states` if the name is `team_state`, else to `events`. Comment lines (`: ping`) are ignored. Both channels hold 64 and close when the body ends.
- `stream.next(t, what)`: the next `team` or `no_team` event, failing after 3 s or if the stream closed. `stream.nextState(t, what)` likewise for the next `team_state`.
- `stream.seen()`: the names of every event received so far, across both channels, in arrival order (for checking order between `team` and `team_state`).
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
| `TestTokenAndAuthErrors` | `tests/auth_test.go` | expired and never-issued codes are `400 invalid_code`, an empty code `400 invalid_body`; `DeleteExpired` removes exactly the expired one; no token is `401 missing_token`, a garbage token `401 invalid_token`; `/healthz` is `200`; `POST /api/me` is `405` |
| `TestSignInRefreshesTeamProfile` | `tests/auth_test.go` | a team creator signs in again with a new name and no picture: `GET /api/teams/me` shows the new name and the old avatar, with no restart |
| `TestEmailInUse` | `tests/auth_test.go` | another account already has `player@snu.edu.in`: the callback lands on `reason=email_in_use` |
| `TestStateAndAccessTokensNotInterchangeable` | `tests/auth_test.go` | the OAuth state cookie used as a bearer token is `401 invalid_token`; an access token used as the state cookie lands on `reason=state_invalid` |
| `TestTeamLifecycle` | `tests/teams_test.go` | every teams rule and code on the way from create to start: `401`, `not_in_team`, name trimming, `already_in_team`, `team_full`, `team_not_found`, `invalid_team_id`, `leader_cannot_ready`, `invalid_body`, `not_leader`, `not_all_ready`, `team_not_full`, rejoin resets ready, `team_locked` for start/leave/ready/join in game; members carry no email |
| `TestLeaderLeaving` | `tests/teams_test.go` | leadership passes to the longest-standing member; the new leader can't ready; the last one out deletes the team |
| `TestTeamNameValidation` | `tests/teams_test.go` | empty, spaces, `teams.name_max_length` + 1 characters (33) and a newline are `400`; `teams.name_max_length` × `é` and punctuation are `201` |
| `TestConcurrentJoinsRespectTeamSize` | `tests/teams_test.go` | 2 × (`teams.size` − 1) = 10 concurrent joins into a team of 1: exactly `teams.size − 1` succeed, the rest `team_full` |
| `TestConcurrentStartAndLeave` | `tests/teams_test.go` | 10 rounds of start racing a leave: either started with `teams.size` (6) and `team_locked`, or left with `teams.size` − 1 and `team_not_full` |
| `TestUnroutedRequestsUseEnvelope` | `tests/teams_test.go` | `GET /api/nope` with and without a token → `404 not_found`; `DELETE /api/teams` → `405 method_not_allowed` |
| `TestSelectAndScanRoutes` | `tests/teamstate_test.go` | seed + `restart`; `not_in_game`, `not_in_team`, `invalid_body` (wrong type, empty, over `server.max_body_bytes`), `unknown_shape`, `unknown_waypoint`, `no_shape_selected`; a square with a rejected retry and rescan (`waypoint_already_scanned`), `member_already_scanned`, `pattern_completed`; a wrong scan (waypoint `1012`, inserted in no pattern) accepted as `scan_added` and locking the member; state after; reselect; clear |
| `TestGameShapes` | `tests/teamstate_test.go` | seed + `restart`; `GET /api/game/shapes` without a token is `401 missing_token`; with one (a player in no team): 5 shapes, `square` with 18 patterns and `points` `[4]`, and `scan_window_ms`, `scan_grace_ms`, `max_pattern_points` equal to `config.toml`'s |
| `TestSimTeamCompletesUnderLatency` | `tests/simulation_test.go` | four simulated players scan a square at once, each with its own delays up to 150 ms each way: exactly one `pattern_completed`; every client ends with an empty stack, the square completed and its countdown reset |
| `TestSimLastScanAtDeadlineWithinGrace` | `tests/simulation_test.go` | the last player at 300 ms each way (0.6 s round trip ≤ the 1 s simulated grace) sends 20 ms before their own countdown ends: `pattern_completed` |
| `TestSimLatencyBeyondGraceLosesBottomScan` | `tests/simulation_test.go` | the same at 700 ms each way (1.4 s > 1 s): the bottom scan is stale on arrival, `scan_added` |
| `TestSimGateClosesThenReselectOpens` | `tests/simulation_test.go` | once a countdown runs out the client sends nothing (0 requests); a reselect resets every client's countdown and the square can be scanned |
| `TestSimRaceSameWaypoint` | `tests/simulation_test.go` | two players scan the same waypoint at once: one `200`, one `409 waypoint_already_scanned`; every client converges on a one-scan stack |
| `TestSimStaleScanReachesEveryone` | `tests/simulation_test.go` | real time: after one scan, with nobody acting, every player's stream gets a `team_state` with an empty stack once the scan is older than `simWindow` + `simGrace` (3 s), and every countdown resets |
| `TestTeamEventStream` | `tests/events_test.go` | initial `team` / `no_team`; join, ready, leader leaving (new leader), deletion (`no_team`) arrive live; the stream follows the user into a new team; other users' streams get nothing |
| `TestEventStreamAuthAndCleanup` | `tests/events_test.go` | no token is `401`; `/healthz` counts 1 open stream, then 0 within 2 s of disconnecting |
| `TestStreamIncludesTeamState` | `tests/events_test.go` | `team_state` follows `team` on connect; `GET /api/teams/me/state` works, and is `404 not_in_team` without a team |
| `TestStartPublishesTeamState` | `tests/events_test.go` | a full, ready team with every member's stream open: when the leader starts, each stream gets the `IN_GAME` `team`, then an empty `team_state`, in that order (events seen: `team`, `team_state`, `team`, `team_state`), without reconnecting |

Team sizes and name lengths come from `config.toml` through `e.cfg.Settings`, never repeated as numbers.

**The simulation tests** (`tests/simulation_test.go`) model each player as a phone: its own request and response delays, its own event stream (each event delivered after the response delay, in order), and `scanGate`, a Go copy of the frontend's countdown (`frontend/src/lib/game/scanGate.ts`): start on the first non-empty stack seen, reset on an empty one, send nothing after `game.scan_window`. They shrink `game.scan_window` to 2 s (`simWindow`) and `game.scan_grace` to 1 s (`simGrace`) through `e.cfg.Settings` and `restart`, so each runs in seconds. The bound they check, response delay + request delay ≤ grace, is derived in `internal/teamstate/README.md` ("The client countdown and latency"). They use real time (`time.Sleep`), not a fake clock, because the delays are network delays over real HTTP.

**Not covered here:** `/healthz` returning `503`, the heartbeat (tests wait at most 3 s; covered in `platform/live`), a slow stream being dropped over HTTP (covered in `platform/live`), `Hub.Close` on shutdown, the `Allow` header's value, and `main.go` itself (subcommands, lock ordering, lost-lock shutdown, sweep loop, timeouts).
