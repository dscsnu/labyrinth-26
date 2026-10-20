# AGENTS.md: how to work on the backend

Rules for anyone, human or agent, adding or changing backend code. Follow them exactly; they're what keeps this codebase uniform. When unsure:
- **Code shape:** copy `internal/teams` (or `internal/teamstate` for game rules, `internal/atlas` for read-only content).
- **Documentation depth:** copy `internal/teamstate/README.md`.

Before you finish any change, run the checklist in [§14](#14-before-calling-a-change-done).

## Contents
1. [The product](#1-the-product)
2. [Architecture at a glance](#2-architecture-at-a-glance)
3. [Use cases: the paradigm](#3-use-cases-the-paradigm)
4. [Dependencies](#4-dependencies)
5. [Where state lives](#5-where-state-lives)
6. [The write-through](#6-the-write-through)
7. [API conventions](#7-api-conventions-apigo)
8. [Database and migrations](#8-database-and-migrations)
9. [Live events](#9-live-events)
10. [Configuration: no hard-coded constants](#10-configuration-no-hard-coded-constants)
11. [Doc comments](#11-doc-comments-on-everything)
12. [README per use case](#12-readme-per-use-case)
13. [Tests](#13-tests)
14. [Before calling a change done](#14-before-calling-a-change-done)
15. [Operational rules](#15-operational-rules)
16. [Adding a use case, step by step](#16-adding-a-use-case-step-by-step)
17. [Team state is the gameplay loop](#17-team-state-is-the-gameplay-loop)

## 1. The product
**Labyrinth 26** is a one-day campus game run by DSC SNU. Teams run around campus scanning QR codes at waypoints to trace shapes on the map.

**The players' journey:**
1. **Sign in** with Google (an `snu.edu.in` account) through the SvelteKit frontend.
2. **Form a team** in the lobby: one player creates it and shares its 6-digit code; others join. Up to `teams.size` (6) players, one leader. Members mark themselves ready, and the leader starts with however many there are (1 or more). From then on the roster is locked to players: nobody joins or leaves. **An admin** can still remove a member from any team, lobby or started (naming the new leader when removing the leader; never the last member), on `/admin`; the removed player is teamless and free to join again.
3. **Play:**
   - **The leader** selects a shape type (square, triangle, …); only the leader selects, restarts or gives up (`403 not_leader`), and every member scans. Each type has many precomputed valid patterns: sets of 1–6 waypoints.
   - **Big patterns are locked.** Patterns of **5** (`game.locked_pattern_points`) or more waypoints unlock once the team has defeated **5** (`game.unlock_after_bosses`) bosses. Until then a type made only of big patterns can't be selected (`shape_locked`), and a scan that could only grow into a big pattern is a wrong scan. `team_state.unlocked_points` and `locked_shapes` say what's open (`GET /api/game/shapes` stays the same for everyone).
   - Members **scan waypoints**. There's **no time limit**, and any member may scan any number of the points.
   - A scan of a waypoint that's already in the attempt is **rejected** (`waypoint_already_scanned`).
   - A scan that can't be part of any pattern of the type (with the scans already in) is **accepted, marked wrong, and reported** (`wrong_scan`, `stack[].wrong`, `poisoned`). It **poisons** the attempt: every later scan is refused (`selection_poisoned`) until the leader selects again.
   - When the scans are exactly one valid pattern, it **completes** (a cast, logged in `team_cast` and listed by `GET /api/teams/me/casts`). Every new selection and every completion **purges** the scans.
   - **Every cast damages the team's current boss.** All teams fight the same **6** (`game.bosses`) bosses in serial order (`GET /api/game/bosses`: name, asset, description, HP). A cast deals its pattern's damage × the team's multiplier (1 unless something sets it; nothing does yet), rounded. Reaching the boss's HP defeats it and moves the team to the next; the excess is lost. After the last boss, casts still count but deal nothing.
   - **Bosses attack back.** Every team starts with **10** (`game.health`) health. Every **10 min** (`game.boss_attack.interval`) of a team's phase against its current boss (from its start, or the cast that defeated the previous boss), the boss takes its damage off the team's health (`[game.boss_attack.damage]`, by serial: its serial number for now): the server applies and publishes it with nobody acting (`boss_attacked`). At 0 the team is **knocked out** (`knocked_out`): it can't select or scan until an admin revives it (`POST /api/admin/teams/{team_id}/revive`, full health and a fresh clock).
4. Progress (patterns and shapes completed, waypoints visited, the cast log, bosses defeated, health) is the team's score.

**Scale and constraints:**
- Up to ~10,000 players on one day, all at once.
- Phones on campus Wi-Fi: slow, retrying, reconnecting. Every rule must be safe under retries and races.
- One backend process. Game state lives in its memory, and every change is written through to Postgres.
- Live updates go to phones over server-sent events (SSE).

**Words** (the client and the code use different names; use the code's in code, and both in docs where they meet):

| Client says | Code says | Meaning |
|---|---|---|
| type | `Shape` | square, triangle, …; what a team selects |
| shape | `Pattern` | one valid set of 4-digit waypoint IDs of a type |
| point | `Waypoint` | a QR location, 4-digit ID |
| buffer | `Stack` | a team's accepted scans toward the selected type |
| spell, powerup (the `feat/damage-lookup` PR) | `Spell` | what completing a pattern casts: the team's reward, one per pattern |

## 2. Architecture at a glance
```
browser ──▶ SvelteKit server (frontend/) ──Bearer JWT──▶ Go backend (backend/) ──▶ Postgres
   ▲                                                      │  memory: atlas, teams, team state
   └────────────────────── SSE (/api/events) ◀────────────┘
```
- **Go 1.26**, standard `net/http` with method patterns. **pgx/v5**. No ORM, no Supabase, no web framework.
- **Auth:** Google OAuth with PKCE → one-time login code → the SvelteKit server swaps it for an **Ed25519 JWT** (`EdDSA`). The frontend verifies tokens locally with the public key.
- **Memory first:** hot data (teams, team state) is held in memory, read lock-free, and written through to Postgres on every change. Static content (the atlas) is loaded once and never written.
- **One instance per database**, enforced by an advisory lock that is watched while serving.

**Repo layout:**
```
backend/
  main.go                   entry point: subcommands, startup and shutdown order
  config.toml               every tunable value (§10)
  AGENTS.md, README.md
  cmd/docaudit/             checks every declaration has a doc comment (§11)
  cmd/loadtest/             10k-player load test against a bench database
  internal/
    atlas/ auth/ user/ teams/ teamstate/ qr/   use cases (§3)
    server/                 wires use cases together; cross-cutting routes
    platform/               shared plumbing, no business rules:
      config/ httpx/ identity/ live/ postgres/
frontend/                   SvelteKit app (separate rules)
```

## 3. Use cases: the paradigm
Every feature belongs to exactly one **use case**: one package under `internal/` that owns its data, its rules and its routes.

**Current use cases:**

| Use case | Owns | State |
|---|---|---|
| `auth` | Google sign-in, login codes, JWTs, the auth middleware | Postgres only |
| `user` | user profiles and roles | Postgres only |
| `teams` | teams, membership, ready, start | memory, written through |
| `teamstate` | selection, scan stack, completions, boss progress, health and its attack timers | memory, written through |
| `atlas` | static content: waypoints, shapes, patterns, spells | memory, read-only, loaded once |
| `bosses` | static content: the boss sequence (name, asset, description, HP, attack interval and damage) | memory, read-only, loaded once |
| `qr` | rotating waypoint QR tokens: mint (staff) and verify | none (stateless; derived from the secret and the clock) |

**Files**, the same in every use case:
```
internal/<usecase>/
  doc.go      package comment, pointing to README.md
  api.go      HTTP only: decode, validate input shape, call read/write, encode, map errors
  read.go     types, and every read
  write.go    every write; the write-through steps if state is in memory
  db.go       all SQL for this use case, and nothing else
  README.md   the full documentation (§12)
  store.go    ONLY if state is in memory: layout, Load, persist, reconcile, publish
  tests/      the use case's tests (§13): doc.go + *_test.go, package tests
```
- **No test files beside the code.** A use case's directory holds only code and its README; every test lives in its `tests/` subpackage (§13). The same goes for `platform/` packages and `server/`.
- **The four main files always exist** (`api.go`, `read.go`, `write.go`, `db.go`), even when one is small.
- **A read-only use case** (static content, no runtime writes; `atlas`, `bosses`) is the one exception: it has **no `write.go`**, and **no `api.go` unless it serves a route of its own** (`bosses` serves `GET /api/game/bosses`; `atlas` has none). Its README says so in those sections. It still has `doc.go`, `read.go`, `db.go`, `store.go` (the layout and `Load`) and `README.md`.
- **Supporting files** are fine when each holds one clear thing and the README lists it: `google.go` (auth), `sets.go` and `geometry.go` (atlas).
- **Where code goes:**
  - SQL only in `db.go`.
  - HTTP only in `api.go`.
  - Business rules only in `write.go` and `read.go`. Never in `api.go` or `db.go`.
- **Names:** packages short and lowercase (`teams` is plural to match its routes). Exported types are the use case's nouns (`Team`, `Member`, `State`, `Stack`).

## 4. Dependencies
- **A use case may import:**
  - `internal/platform/...`
  - another use case's **read API**, through a small interface it declares itself (teamstate's `Teams`, and its `Atlas`, which `*atlas.Atlas` satisfies). Plain types in that API may be used directly (teamstate's `State` holds `atlas.WaypointSet`; it reads `teams.Team`)
- **A use case never imports** another's `db.go` types or writes. If a write in one must update another's memory or publish another's state, `server.Build` passes a callback (auth's `onSignIn` → `teams.RefreshProfile`; teams' `Rules.OnStart` → `teamstate.PublishTeam`, which sends a started team its `team_state` and arms its boss attack timer; teams' `Rules.OnRosterChange` → `teamstate.RosterChanged`, which drops teamstate's cached roster after an admin removes a member from a started team).
- **No import cycles.** Shared pieces move into `platform/` (that's why `platform/identity` sits between auth and everything else).
- **`platform/` never imports a use case.**
- **`internal/server/server.go` is the only place** that constructs use cases and registers routes, in dependency order. `server.Build` loads the atlas first (every game use case reads it), then teams, then teamstate.

## 5. Where state lives
- **Postgres only** (`auth`, `user`): for data that's rarely read or written. `read.go` and `write.go` call `db.go` directly. Say so in the README, and why.
- **In memory, read-only** (`atlas`, `bosses`): for static content read on every request and never written at runtime. `Load` builds it once from Postgres and validates it; it's **immutable** once `Load` returns, so reads take **no locks**, and there's no entry/snapshot machinery, write-through or reconcile. Anything derived (indexes, lookup maps, computed values like spells) is rebuilt by `Load` on every start and listed in its README's Memory ↔ Postgres table. Content changes need a restart.
- **In memory, written through** (`teams`, `teamstate`): for data read or written constantly during the event. Then:
  1. **`store.go` holds the layout:**
     - an index map behind a `sync.RWMutex`
     - one `entry` per key: a `sync.Mutex` for writers and an `atomic.Pointer[T]` to an **immutable snapshot**
     - draw the layout in a comment block
  2. **Reads load the snapshot** lock-free, or with only a brief shared lock on the index. **Reads never touch Postgres.**
  3. **Snapshots are never modified.** Copy, change the copy (`slices.Clone` any slice you change), swap the pointer. Prefer fixed-size values (bitsets, arrays) so a copy is a plain struct copy.
  4. **Lock order:** entry lock, then the index lock. Never hold the index lock during I/O.
  5. **`Load(ctx, pg, …)`** builds memory from Postgres at startup and must reproduce it **exactly**. Prove that with a restart test that compares JSON before and after.
  6. **Anything held only in memory** (not rebuilt by `Load`) must be listed in the README's Memory ↔ Postgres table, with why that's acceptable.

## 6. The write-through
Every write in an in-memory `write.go` runs these steps, in this order, **each marked by a numbered comment**:
```go
// 1. lock      the entry
// 2. validate  rules against the in-memory snapshot
// 3. build     the next snapshot (a copy)
// 4. persist   via db.go, inside persist(ctx, func(ctx) error {...})
// 5. apply     swap the snapshot, update indexes
// 6. publish   live events, still holding the entry lock
```
**Allowed variations**, and only these. Each keeps its step numbers, and its comment says which variation and why (`// 4. persist, before 3. build: Postgres sets joined_at`):
- **(a) Lock-free checks first.** Checks that need no lock may run as `2. validate` before `1. lock`: static content, and an `IN_GAME` team's status (it never goes back). **Not the roster:** an admin can remove a member from a started team, so a write that acts for a member checks membership again under its lock (teamstate's `isMember`). Checks against the entry's snapshot still run under the lock, labelled `2. validate` again (teamstate's `Select`, `ClearSelection`, `Scan`).
- **(b) Persist before build.** When the database generates part of the new value (a `joined_at`, `started_at`, `created_at`, or a generated key), `4. persist` may come before `3. build`, so the snapshot is built from what was stored (teams' `Create`, `Join`, `Start`). Still nothing is applied before persist succeeds.
- **(c) Nothing to lock on create.** A write that makes a new entry has nothing to lock until it exists. Its comment says `1. lock: none` and names what guards the race instead, usually a unique constraint (teams' `Create`: `teammember.user_id`).
- **`persist`** detaches from the request context and is capped at `storage.write_timeout`, so a client disconnecting can't leave a commit's outcome unknown.
- **On a persist failure, call `reconcile`:** reload that key from Postgres (Postgres wins), publish the corrected state, and return the original error. **Never apply after a failed persist.**
- **Rejections and no-ops return before step 4:** nothing written, nothing published.
- **A memory-only change** skips step 4 and says so in its doc comment. Only two kinds qualify:
  - data another use case already saved (`teams.RefreshProfile`: the user use case wrote the profile)
  - a change `Load` reproduces from what's already stored: a **time-driven change derived from stored state** (teamstate's `catchUp`: a boss attack. Health isn't stored per attack; it's derived from the stored phase, `phase_started_at` and `phase_health`, and the clock, so `Load` recomputes it exactly). Its comment says `4. persist: none, memory-only`, and it still bumps the version and publishes. Prefer this shape for anything time-driven: store the moment it started, derive the rest, and let only the events people cause write
  - **Every write applies due time-driven changes first** (teamstate's writes call `catchUp` right after `1. lock`), so a late timer never loses one and every write validates against the real state
- **The database is a backstop:** mirror invariants as constraints (unique, check, foreign key), and map violations to rule errors in `db.go` (`mapMemberInsertErr`). When a rule can't be a constraint (it depends on other rows' history, like teamstate's stack), say so in the README's concurrency section.
- **`execOne`** for writes that must affect exactly one row, so memory/Postgres disagreement is detected rather than ignored.
- **Multi-statement writes** go in one `pgx.BeginFunc` transaction.

## 7. API conventions (`api.go`)
- `type API struct`, `NewAPI(...)`, and `Routes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler)`. Use method patterns: `"POST /api/teams/join"`.
- **Who's calling** comes from `identity.UserID(r)`. Never trust an ID from the body for "who am I".
- **Admin routes** live under `/api/admin/…`, registered by the owning use case's `AdminRoutes(mux, admin)` (teams: list and remove members; teamstate: revive). `server.Build`'s `admin` wraps a handler as `authAPI.RequireAuth(userAPI.RequireAdmin(h))`: `RequireAdmin` reads the role from Postgres (`user.Get`) on **every** request, never from the token (up to `auth.token_ttl` stale), and answers `403 not_admin` otherwise. Log every admin write with the admin's ID. An admin acting on a player takes IDs from the path (`/api/admin/teams/{team_id}/members/{user_id}`), not the body.
- **Auth middleware:** every route uses `authAPI.Authed` / `RequireAuth` (Bearer header only). `RequireStreamAuth`, which also accepts the `labyrinth_token` cookie, is for `GET /api/events` **only**: a browser's `EventSource` can't send headers. Never put it on a route that changes anything, or on any other route: a cookie is sent by the browser automatically, so a cookie-authenticated write could be triggered by another site (CSRF). `TestStreamCookieAuth` checks that reads and writes elsewhere refuse the cookie.
- **Bodies:** `httpx.DecodeJSON` (rejects unknown fields). The server caps every body at `server.max_body_bytes`. Use pointer fields for required booleans.
- **OpenAPI:** every route, request body, response schema and error code is in `internal/server/openapi.yaml`. Add or change it in the same change as the route. List a response's error codes in its `description` as ``Codes: `a`, `b` ``: `internal/server/tests/openapi_test.go` fails if a registered route or a sent code/status pair is missing from the spec, or the spec has one the code doesn't. It can't check schemas field by field, so keep them in step with the Go types yourself.
- **Responses** always use the envelope via `httpx.OK`, `httpx.JSON`, `httpx.Error` or `httpx.ErrorCode`: `{"success", "code"?, "message"?, "payload"?}`. Unknown routes and wrong methods already get `404 not_found` and `405 method_not_allowed` envelopes from `server`.
- **Errors:**
  - Rule errors are `var ErrX = errors.New("…")` in `write.go` or `read.go`.
  - `api.go` maps them in **one `errorCodes` table** to a status and a **stable snake_case code** the frontend switches on (`team_full`). **Every 4xx has a code.** Only `500 internal error` has none (logged).
  - Statuses: `400` bad input (`invalid_body`, `invalid_<field>`), `401` auth (`missing_token`, `invalid_token`, and `user_not_found` when a valid token's user no longer exists: the caller must sign in again), `403` not allowed (`not_leader`), `404` missing (`not_in_team`), `409` state conflict (`team_full`, `waypoint_already_scanned`).
  - The same code may carry a different status where the meaning differs: `user_not_found` is `401` on `GET /api/me` (the caller's own token names a deleted user) but `404` on `POST /auth/token` (a login code's user is gone; no token is involved). Document both in the README.
  - Only envelope errors go in the table. Redirect reasons (auth's callback, `/auth/error?reason=…`) and the middleware's `missing_token` / `invalid_token` are written where they happen.
- **Retries are normal.** Decide what a resent write does and document it in the case tables. teamstate rejects a resent scan with `409 waypoint_already_scanned` and changes nothing; the app treats that as "already in".
- **Never expose** other users' emails or internal keys.

## 8. Database and migrations
- **One `db` struct** per use case, holding the pool, with unexported methods named `selectX`, `insertX`, `updateX`, `deleteX`.
  - `upsertX` is allowed for one `INSERT … ON CONFLICT … DO UPDATE` (`upsertGoogle`, `upsertSelection`).
  - A multi-statement transaction may take a verb that names the operation better than any one statement (`removeMember`, `consumeCode`).
  - Either way, the **doc comment lists each statement** it runs, in order, and which run conditionally.
- **"No rows" maps to `postgres.ErrNotFound`.**
- **Time comparisons happen on one clock.** If Postgres checks an expiry with `now()`, Postgres sets it with `now() + $ttl`.
- **Migrations** are `internal/platform/postgres/migrations/NNNN_name.sql`, numbered after the last one, embedded in the binary.
  - **Never edit an applied migration.** Each one's SHA-256 is recorded, and the backend refuses to start if a file changed (`ErrMigrationEdited`). Add a new migration instead, even for a wrong comment (`COMMENT ON` fixes it in the database; see `0005`).
  - Use fixed IDs for anything printed or referenced outside the database (waypoint IDs, team codes).
  - Static content (waypoints, shapes, patterns) is read at startup by `atlas`; edits need a restart.

## 9. Live events
**One stream per signed-in user** (`GET /api/events`) carries every event, at every scope. A client opens exactly one connection; nobody subscribes to channels.

- **Three scopes, one way to publish each.** The publisher decides who gets an event, when it sends it:

  | Scope | Publish with | Reaches | Examples |
  |---|---|---|---|
  | user | `hub.Send(userID, ev)` | that user's streams | `no_team` |
  | team | `hub.Send` for each current member | the team's members | `team`, `team_state` |
  | game | `hub.Broadcast(ev)` | every open stream | future quizzes, quick-time events |

  Publish after apply and **while holding the entry lock** of the state the event shows, so one key's events stay in order. Read the recipients from memory at send time (or a cached roster, as teamstate does, dropped by the owner's hook whenever it changes: `RosterChanged`); never make clients resubscribe.
- **Every event's data is the envelope `{"type": …, "message": …}`.** The SSE event name is the resource; `type` is why it was sent; `message` is the resource's **full** state, never a diff. A reconnecting client needs nothing replayed. Clients render from `message` and use `type` only for moments (an animation, a toast), so a missed event costs nothing.
- **Types** (`live.EventType`) are snake_case causes, past tense where it reads naturally (`member_joined`, `spell_cast`). Declare them as exported constants in the use case that publishes them, documented with when they're sent. `snapshot` (on connect) and `resync` (after a reconcile) are reserved, declared in `platform/live`, and used by every resource. Never put state in `type`: it says what happened; `message` says what is. Adding or renaming a type is an API change: update the server README's type table and the `…Event` schema's enum in `openapi.yaml`.
- **Build each event once with `live.NewEvent(name, type, message)`,** then send that value to every recipient: its wire bytes are shared, not re-encoded per stream. `live.MustNewEvent` only for constant messages.
- **Name events after the resource,** in snake_case (`team`, `no_team`, `team_state`); `message` is the same JSON as the matching GET route.
- **Versioned resources carry `version: live.Version`** (`team`, `team_state`), so clients keep the newest copy from the stream and from write responses alike. Every apply sets `next.Version = cur.Version.Next(epoch)` in its build step; a reconcile continues from the in-memory version (never back); no-ops and resends (`PublishTeam`) don't bump. `epoch` is one per process, set in `server.Build` and passed through `Rules.Epoch`. Version is memory-only: list it in the README's Memory ↔ Postgres table.
- **Game-wide resources** send `null` when nothing is active, or an object with at least an `id`. A broadcast payload is identical for every viewer: anything that varies per player goes in that player's scope (a player's own answer), and a team's results go in `team_state` (§17).
- **Hot events stay small.** Data that never changes while the backend runs (atlas content, game rules) comes from GET routes once and never rides in events.
- **Never publish** on a rejection or no-op.
- **A new event that must arrive on connect** needs a snapshot on the use case (`Snapshot(userID)`, or `Snapshot()` for game scope), added to `server.snapshot` in its place: `hello`, then user and team events, then game-wide ones.
- **The server owns time.** A change caused by time passing is applied and published by the server when it happens: teamstate arms one `Clock.AfterFunc` timer per started team for its boss's next attack and publishes `boss_attacked` / `knocked_out` with nobody acting. Timers are memory-only: `Load` re-arms them, `Close` stops them (on shutdown, and in tests before a second store loads), and a timer's callback takes the entry lock like any write. Clients never decide state from time: they count down to `boss.next_attack_at` for display, and the attack arrives as an event. See §17.
- **`hello {server_time}` is the one way clients measure time against the game.** Every stream opens with it, so the offset (server time − the client's monotonic clock) is re-measured on every connect (`frontend/src/lib/game/serverClock.ts`). Anything a client shows relative to now ("scanned 2 min ago", the countdown to `boss.next_attack_at`) uses it, so every phone agrees. `hello` carries nothing else; game rules come from `GET /api/game/shapes`.
- **Admin-triggered game events** read the caller's role from `user.Get`, never from the token (roles in tokens can be stale): use an admin route (§7, `RequireAdmin`), then follow §6 with `Broadcast` as step 6.
- **The wire-level reference** (endpoint, scopes, the envelope, every event and type, triggers, ordering, versioning, heartbeat, reconnection, client integration) is [`internal/server/README.md`, "The event stream"](internal/server/README.md#the-event-stream). Update it with any change to what is published, or when.

## 10. Configuration: no hard-coded constants
- **Every tunable lives in `backend/config.toml`:** sizes, counts, durations, timeouts, limits, intervals. Never write `30 * time.Second` or `6` in a use case, a test, or a doc comment.
- **Missing, unknown and out-of-range keys** are all startup errors, so the file and the code can't drift.
- **To add a value:**
  1. Add the key to `config.toml`, with a comment saying what it does and its allowed range.
  2. Add the field (`toml` tag and doc comment) to the matching struct in `internal/platform/config/settings.go`, and a range check in `validate`.
  3. Add it to `validTOML` in `internal/platform/config/tests/settings_test.go`, and a `TestLoadSettingsRejects` case if it can be invalid.
  4. Pass it into the use case through its `Rules` (or constructor) in `internal/server/server.go`. **Use cases never read the file.**
  5. In tests, read values with `configtest.Settings(t)`, never by repeating the number. If a test only makes sense for some values, skip with a named minimum (`minTestTeam`).
  6. In docs, write the value with its key: **6** (`game.max_pattern_points`).
- **Allowed in code**, each as a **named constant** whose comment says why it isn't in config.toml:
  - formats and security parameters (4-digit waypoint IDs, 6-digit team codes, 256-bit random tokens)
  - Go type sizes (64-bit sets, `config.MaxTeamSize`)
  - correctness bounds (lock retries)
- **Tooling** (`cmd/loadtest`) takes tunables as flags, with internal timeouts as named constants.
- **Secrets and deployment URLs** live in `.env`, never config.toml.

## 11. Doc comments: on everything
Follow Google's Go style for doc comments.
- **Every declaration gets one:** package, type, func, method, const, var, struct field where it isn't obvious, exported **and unexported**, including tests and `cmd/`. In a `const (…)` or `var (…)` group, every item gets its own.
- **Start with the identifier's name**, in full sentences: `// Join adds userID to a LOBBY team that has room.`
- **Say what it does, what it returns, and every surprise:** errors, no-ops, locking requirements, what it doesn't do.
  ```go
  // Redeem uses up a code, calling use with its user. The code is used up only
  // if use returns nil; otherwise it stays redeemable and use's error is
  // returned. postgres.ErrNotFound if the code is unknown, expired or already
  // used.
  ```
- **Handlers** name their route: `// join handles POST /api/teams/join {"team_id"}: the caller joins that team.`
- **Tests:** `// TestX checks that …`. Benchmarks: `// BenchmarkX measures …`.
- **Package comment** in `doc.go`: what the use case owns and where its state lives, ending with `See README.md for every rule and case, and how each file works.`
- **Inline comments explain why**, not what: write-through step markers, lock reasons, ordering constraints, measured performance choices (see `FitsShape`).
- **Performance claims are measured,** with the numbers and the data they were measured on.
- **Check:** `go run ./cmd/docaudit` must print `docaudit: ok`.

## 12. README per use case
Each use case's `README.md` documents it **completely enough to rebuild it without reading the code**. `internal/teamstate/README.md` sets the depth. Sections, in this order (skip only ones that truly don't apply, e.g. no write-through for a Postgres-only use case; a read-only use case keeps the write-through, API and concurrency headings with one line saying why they don't apply):

1. **Title and one line** on what the use case owns; a note that config values appear with their key.
2. **Concepts:** a table defining every term the rules use, including client vocabulary.
3. **The flow:** numbered steps of how a user moves through it, and the reasons behind non-obvious choices ("Why …").
4. **How each file works:** one subsection per file. Every function or group, what it does, what calls it; for `db.go`, exactly which tables each write touches in which transaction.
5. **Data model: memory** (if in memory): every struct, field by field, with its Go type and meaning; the measured snapshot size; why the layout is what it is.
6. **Data model: Postgres:** every table read or written, with its migration, columns and types, and each constraint with **the rule it backs**.
7. **Memory ↔ Postgres:** each table or query mapped to the in-memory field it rebuilds; what isn't persisted, and why a restart reproduces memory exactly.
8. **Walkthroughs:** worked examples with **concrete values** (IDs, times, bitmasks), one row per request, showing the check, memory after, Postgres after and the response. One per tricky case (timeouts, retries, boundaries).
9. **The write-through** (if in memory): the six steps, which `db.go` call each write uses, failure, and which requests are rejections or no-ops.
10. **API:** a routes table, **full JSON examples** of every request, response and error, every field explained, and when live events are sent.
11. **Rules and cases:** a table per operation listing **every** case **in validation order**, with its exact result (status and code, or behaviour). Include no-ops, retries, boundaries (exactly at a limit and just past it), config-driven limits, and startup refusals.
12. **Concurrency and locking:** every race and who wins, lock order, and what's enforced only in memory.
13. **Failure and recovery:** database errors, disagreement, outage, restart, config changes between restarts.
14. **Limits and how to change them:** every limit, where it lives (config key or type size), how to raise it.
15. **Not decided yet:** open questions and known gaps (security gaps marked ⚠).
16. **Tests:** a table of every test and what it covers.

**Extra sections** specific to a use case go after the data model sections (after 7), in the Contents like the rest: atlas's "How waypoints and patterns are stored" and "Spells", teamstate's "Wrong scans".

**Style:** short sentences, tables over paragraphs, real values over placeholders.

**Keep docs true:** update the README in the same change as the code, along with `backend/README.md` (endpoints, configuration), `internal/server/openapi.yaml` (routes and codes) and `internal/platform/README.md` or `internal/server/README.md` when you touch those. A bug found and not fixed goes under "Not decided yet" until it is.

## 13. Tests
- **Where tests live:** `internal/<package>/tests/`, a separate package named `tests`, holding a `doc.go` (package comment: whose tests these are) and the `*_test.go` files (Go only runs tests from files with that suffix). Run one package's with `go test ./internal/<package>/...`.
- **Tests are black-box:** they import the package and use only its exported API, so they test exactly what other packages rely on, and refactoring internals doesn't break them.
- **Exporting a seam for a test** is allowed only when it's a sensible API in its own right, documented like any other: `atlas.New(Source, Rules)` (build an atlas from given content, which `Load` also uses), `teamstate.Rules.Clock` (a `teamstate.Clock` interface, `Now()` and `AfterFunc(d, f) (stop func() bool)`; nil means the real one, `time.Now` and `time.AfterFunc`. Tests pass a fake whose timers fire only when the test advances it, never from inside `AfterFunc`), `teamstate.Store.Close`, `teamstate.NewStack`, `teams.Store.All()`, `auth.Issuer` / `auth.AccessAudience` (the frontend must match them). Never export a field or a "ForTesting" function just to reach inside. If a behaviour can only be reached through internals, test it end to end instead (auth's state-vs-access token tests live in `internal/server/tests`).
- **Test every edge case.** Every rule, error code and boundary in the README's case tables gets a test: exactly at a limit and just past it, retries, concurrent duplicates, the smallest and largest content (1-point and 6-point patterns), and config values from `configtest.Settings`.
- **Store tests** run against real Postgres via `pgtest`, each package in its own schema:
  ```go
  db := pgtest.Open(t, pgtest.URL(t, "<usecase>_test"))
  ```
  Time-dependent rules use a fake clock passed in through `Rules.Clock`, never `time.Sleep`.
- **End-to-end HTTP tests** live in `internal/server/tests/`: `setup(t)`, `newPlayer`, `signIn`, `call`, `expect`, `openStream`, `restart`.
- **In-memory stores always get:**
  - a restart test (reloaded memory equals memory, compared as JSON)
  - a reads-work-with-the-database-closed test
  - a reconcile test (Postgres edited by hand, the next write fails and memory corrects)
  - a race test for every invariant concurrency could break (`TestConcurrentJoinsRespectTeamSize`, `TestConcurrentRescansAddOneScan`)
- **Hot reads get a benchmark,** and a result sink so the compiler can't discard the work.
- **The game loop has two 100-team simulations; new gameplay joins both.**
  - `internal/teamstate/tests/gameloop_test.go` (`TestGameLoopSimulation`): 100 teams of every size play 90 minutes of game time on the fake clock, each by one of 12 strategies, checked after every step against `model`, an independent re-implementation of the rules written from the README (not copied from the code), with counters that fail if a configuration never happened. `TestGameLoopRestart` plays the same league with the backend down part way (0, 90 s, 15 min, 1 h of game time passing while it's down), then restarts it: every team must equal its model at the new time, the attacks that fell due while down included. `BenchmarkGameLoopCast` measures casts per second with 100 teams at once.
  - `internal/server/tests/simulation_test.go` (`TestSim100Teams`): 100 teams over real HTTP and event streams with per-phone delays, every request's status and code checked, every phone converging on its team's state by `version`.
  - **A gameplay change isn't done** until the model knows the rule, a strategy exercises it (with a counter, so it can't silently stop happening), and the HTTP simulation covers any new route or code. A mismatch with the model is a bug in one of them: decide which from the README, never by loosening the check. (The simulation found that a knocked-out team's `boss.attacks` changed across a restart.)
- **Postgres-only use cases** still get store tests for their SQL (`internal/user/tests/store_test.go`, `internal/auth/tests/codes_test.go`).

## 14. Before calling a change done
Run from `backend/`:
```bash
gofmt -l .                       # prints nothing
go vet ./...
go run ./cmd/docaudit            # docaudit: ok
docker run -d --rm --name laby-test-pg -e POSTGRES_USER=t -e POSTGRES_PASSWORD=t \
  -e POSTGRES_DB=t -p 55432:5432 postgres:17-alpine
TEST_DATABASE_URL="postgres://t:t@localhost:55432/t?sslmode=disable" go test -race -count=1 ./...
```
- **CI runs this checklist** on every push to `main` and every pull request (`.github/workflows/ci.yml`), **one job per use case**:
  - `lint`: gofmt, vet, docaudit.
  - `plan`: lists every package with tests (`go list`, the ones with `_test.go` files) and turns them into the matrix: `internal/<use case>/tests` becomes `test (<use case>)` (`test (teamstate)`, `test (platform/live)`, `test (server)`). Nothing to edit when a use case is added: its first test adds its job. (`jq` builds the JSON; it's preinstalled on GitHub's runners, not necessarily on your machine.)
  - `test (<use case>)`: that package's tests with `-race -count=3 -v`, against its own throwaway Postgres, failing on any `--- SKIP`. `fail-fast` is off, so every use case reports.
  - `backend`: green only if `lint` and every `test` job passed. **Make this the one required status check** in branch protection: the per-use-case job names change as use cases are added, this one doesn't.
  - `frontend`: `bun run check` and `bun run test`.
  - Don't merge a PR with a red CI run.
- **Run the race tests three times**; a flaky pass is a fail.
- **Check tests ran:** with `-v`, nothing may be `SKIP`.
- **Hot-path changes:** `go run ./cmd/loadtest -players 600 -rush-window 3s` with 0 errors.
- **Docs:** the README sections in §12 match the code, every new test is in the Tests table, every new config key is in `config.toml` and the READMEs, and every route change is in `openapi.yaml`. A gameplay change also passes the checklist in §17.
- **Report honestly:** what was verified and how; what wasn't run; bugs found and not fixed.

## 15. Operational rules
- **Exactly one backend process per database.** The instance lock is taken before migrating, seeding or serving, and checked every `storage.instance_lock_check`; if it's lost, the backend shuts down.
- **`migrate` and `seed` refuse to run while a backend serves** the same database. Stop it, run them, start it.
- **Don't edit in-memory data with SQL while the backend runs.** Stop it, edit, start it.
- **Every phone holds a stream open.** Raise the process's file-descriptor limit well above the expected players (one per stream, ~10,000 on game day), keep any proxy's idle timeout above `live.heartbeat`, and route `/api/events` straight to the backend rather than through the SvelteKit server (`internal/server/README.md`, "Endpoint").
- **Never point `TEST_DATABASE_URL` at a real database:** tests drop schemas.
- **⚠ Before the event:** QR verification must replace the raw waypoint ID in `POST /api/teams/me/state/scans`.

## 16. Adding a use case, step by step
1. Decide its state (§5) and write the README's Concepts, flow, data model and rules tables **first**: they're the spec.
2. Add a migration (§8) if it needs tables.
3. Create the package with `doc.go`, `api.go`, `read.go`, `write.go`, `db.go` (and `store.go` if in memory; no `api.go` or `write.go` if read-only, §3), each declaration documented (§11).
4. Put every tunable in `config.toml` and pass it in through `Rules` (§10).
5. Wire it in `server.Build`, in dependency order, and register its routes (§2, §4).
   Document every route in `internal/server/openapi.yaml` (§7).
6. Add live events and a snapshot if clients need its state live, choosing the scope (user, team or game) by who must see it (§9). Per-team gameplay results don't get their own event: they go inside `team_state` (§17). A game-wide resource (a quiz) gets its own game-scope event, and each team's result from it goes in `team_state`.
7. Write the tests for every case in the README (§13).
8. Finish the README to §12 depth, update `backend/README.md`, and run §14.

## 17. Team state is the gameplay loop
Everything a team's players see about the game loop reaches them as **one struct**. Keep it that way.

- **One struct, sent whole.** Per-team gameplay state lives in `teamstate.State`. Clients get it as one JSON object, the `View`, which is the `team_state` event's `message`. It's sent whole over the event stream on connect and after every change, and it's the response of every teamstate route (`GET /api/teams/me/state`, both selection routes, and `ScanResult.state` on a scan).
- **Clients never poll.** Everything the play screen shows about the team's game comes from the latest `View`. Its other inputs, and the only ones, are:
  - the static `GET /api/game/shapes` (rules and content, fetched once)
  - the static `GET /api/game/bosses` (the boss sequence, fetched once; `team_state.boss` names the current one by serial)
  - `GET /api/teams/me/casts` (the cast log, fetched once on load; each new cast then arrives as `team_state.last_cast` with type `spell_cast`, which the client prepends, and `team_state.cast_count` tells it to re-fetch when it missed some)
  - the `team` event (the roster)
  - `hello` (the clock, §9)
  - **game-wide events** (§9): resources identical for every player, such as a quiz or a quick-time event, each its own game-scope event carrying `null` or an object with an `id`. They're the one deliberate exception to "one struct": something every player sees the same way is one broadcast, not a copy in every team's `team_state`. **What a team does with them** (its answers, its score from them) is per-team, so it goes in `team_state` like any other result.
- **Static vs per-team.** Static definitions belong to a read-only use case: waypoints, shapes, patterns and spell values to `atlas`, the bosses (name, asset, HP, attack interval and damage) to `bosses`. Per-team gameplay state belongs in `State`: a team's boss progress and health (`State.Boss`: `bosses_defeated`, `boss {serial, damage, phase_started_at, attacks, next_attack_at}`, `damage_multiplier`, `health`, `knocked_out`) is in `team_state`, the boss itself isn't.
- **New per-team results go inside `team_state`.** A module with per-team results (points, damage, powerups) surfaces them as fields of `team_state`, as boss progress does. Teamstate reads them through a small interface it declares (§4), the way it reads `Teams` and `Atlas`. Don't add a separate event or endpoint for them. The one exception is a **history** too long to resend on every change (the cast log): a read endpoint for the load, with each new entry carried live in `team_state` (`last_cast`), plus a count (`cast_count`) so a client knows when its copy is incomplete.
- **`team_state` is versioned** (`version`, §9): every apply bumps it, so a client keeps the newest copy whether it came from the stream or a response. Each publish says why in its envelope `type` (`shape_selected`, `scan_added`, `wrong_scan`, `spell_cast`, `boss_defeated`, `boss_attacked`, `knocked_out`, `revived`, …).
- **Only boss attacks change with time alone.** There's no time limit on an attempt. The one time-driven change is a boss attack: the server applies it on a timer and publishes `boss_attacked` / `knocked_out` (§9), derived from the stored phase so it writes nothing (§6), and every write catches up on due attacks first. Clients use timestamps (`scanned_at`, `cast_at`, `boss.next_attack_at`) only for display and countdowns, against server time estimated from `hello` (`serverClock.ts`, §9), never to change the state they show: a countdown reaching zero changes nothing until the server's event arrives.
- **Rejected or no-op requests publish nothing** (§6, §9). That includes `409`s (`knocked_out` among them) and clearing with nothing selected. A wrong scan is not a rejection: it's pushed and published. An attack caught up at the start of a request that's then rejected is its own change, already published.
- **Spells are already in `team_state`.** Clients derive the spell lifecycle from it, with no event of its own: a spell **starts** when `selection` is set, members **finish their part** as `stack` grows (`stack[].user_id` says who, its length how many), and a spell **finishes** when `last_cast` changes (`cast_at`, `shape`, `cast_by`; `dealt` and `boss` say what it did to which boss, `defeated_boss` whether it finished it).

**A new gameplay feature isn't done until it has all of these:**

| # | Where | What |
|---|---|---|
| 1 | `teamstate.State` (`read.go`) | the field, documented |
| 2 | `View` (`read.go`) | its JSON field, built in `view` |
| 3 | Postgres | persisted by the write-through and reloaded by `Load` (`stateOf`), or listed as memory-only in the teamstate README's Memory ↔ Postgres table, with why |
| 4 | publish | every change to it publishes `team_state` with the right envelope `type` (an existing one, or a new constant in `teamstate/store.go`), and bumps `Version` in the build step |
| 5 | `internal/server/openapi.yaml` | the field in the `TeamState` schema, with a description |
| 6 | SSE reference | `internal/server/README.md`, "The event stream": the JSON example, the type table ("Event format") and the triggers table; a new type also goes in `TeamStateEvent`'s enum in `openapi.yaml` |
| 7 | frontend type | `frontend/src/lib/types/Game.type.ts` (`TeamStateSchema`) |
| 8 | tests | a store test, a restart test (JSON equal before and after), and a server or simulation test over HTTP and the stream (`internal/server/tests`) |
| 9 | teamstate README | every section §12 lists that it touches |
