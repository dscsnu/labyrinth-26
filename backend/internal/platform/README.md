# platform

Shared plumbing with no game rules: configuration, the JSON envelope, the caller's identity, server-sent events, and Postgres. Use cases import these packages; these never import a use case.

Every number below that comes from `config.toml` is shown with its current value and its key, e.g. **25 s** (`live.heartbeat`). Change the file, not the code.

## Contents
1. [Packages](#packages)
2. [config](#config)
3. [config/configtest](#configconfigtest)
4. [httpx](#httpx)
5. [identity](#identity)
6. [live](#live)
7. [postgres](#postgres)
8. [postgres/pgtest](#postgrespgtest)
9. [Not decided yet](#not-decided-yet)

## Packages
| Package | Files | What it does | Imported by |
|---|---|---|---|
| `config` | `config.go`, `settings.go` | Reads `.env` variables and `config.toml`, validates both, reports every problem at once. Parses, generates and prints the Ed25519 signing key. | `main`, `server`, `auth`, `teamstate` (`MaxTeamSize`) |
| `config/configtest` | `configtest.go` | Loads the repository's `config.toml` for tests. | tests |
| `httpx` | `respond.go` | The response envelope `{"success", "code", "message", "payload"}` and `DecodeJSON`. | every `api.go`, `server` |
| `identity` | `identity.go` | Carries the signed-in user through a request, so use cases don't import `auth`. | `auth` (sets), everyone else (reads) |
| `live` | `hub.go`, `stream.go` | `Hub`: fans events out to each user's open streams. `Serve`: writes one stream as server-sent events. | `server`, `teams`, `teamstate` |
| `postgres` | `database.go`, `migrate.go`, `instance_lock.go` | The pool, migrations, the single-instance lock, the dev seed, `ErrNotFound`. | `main`, every `db.go`, `server` |
| `postgres/pgtest` | `pgtest.go` | A migrated, empty schema per test package. | tests |

Dependency direction: `platform/config/configtest` → `platform/config`; `platform/postgres/pgtest` → `platform/postgres`. No platform package imports another otherwise.

## config
Two sources, loaded together by `Load()`:

| Source | Holds | Read from |
|---|---|---|
| Environment (`.env` in development) | Secrets and deployment URLs | `os.Getenv`; `main` loads `./.env` first with `godotenv` |
| `config.toml` | Every tunable rule and timing | `$CONFIG_FILE`, else `./config.toml` (relative to the working directory). The Docker image copies it to `/config.toml` and sets `CONFIG_FILE=/config.toml` |

Both are read **once at startup**. A change needs a restart.

### `config.go`
- **`Config`**: `Port`, `DatabaseURL`, `JWTKey ed25519.PrivateKey`, `GoogleClientID`, `GoogleClientSecret`, `PublicURL *url.URL`, `FrontendURL *url.URL`, `AllowedEmailDomains []string`, `Settings Settings`.
- **`Load()`** reads every variable below, then `LoadSettings(SettingsPath())`. Each problem is appended to a list; at the end, if any, it returns `nil` and `errors.Join` of all of them, so one start shows everything that's wrong.
- **`SecureCookies()`**: `true` when `PUBLIC_URL`'s scheme is `https`. Auth uses it for the `Secure` flag on its cookies.
- **`ParsePrivateKey(b64)`**: trims spaces, base64-decodes (standard alphabet), parses PKCS#8 DER, requires Ed25519. Errors:
  - `must be base64 (generate with: go run . keygen)`
  - `not a PKCS#8 private key: …`
  - `must be an Ed25519 key`
- **`GenerateKeyPair()`**: a new Ed25519 pair as base64 PKCS#8 (private, 48 bytes DER → 64 characters) and base64 SPKI (public, 44 bytes DER → 60 characters). Used by `labyrinth keygen`.
- **`PublicKeyBase64(key)`**: the base64 SPKI of `key`'s public half. Used by `labyrinth pubkey`.
- **`getenv(key, fallback)`**: the variable, or `fallback` if unset **or empty**.

### Environment variables
| Variable | Required | Rule | Error if broken | Used by |
|---|---|---|---|---|
| `PORT` | no, default `3000` | not validated | — | `main`: listen on `":" + PORT` |
| `DATABASE_URL` | yes | non-empty; its format is checked only when `postgres.Connect` parses it | `DATABASE_URL is required` | `main` |
| `JWT_PRIVATE_KEY` | yes | `ParsePrivateKey` | `JWT_PRIVATE_KEY is required`, or `JWT_PRIVATE_KEY: <parse error>` | `auth.NewTokens` |
| `GOOGLE_CLIENT_ID` | yes | non-empty | `GOOGLE_CLIENT_ID is required` | `auth.NewGoogle` |
| `GOOGLE_CLIENT_SECRET` | yes | non-empty | `GOOGLE_CLIENT_SECRET is required` | `auth.NewGoogle` |
| `PUBLIC_URL` | yes | trailing `/` trimmed; must parse with a scheme and host | `PUBLIC_URL is required` / `PUBLIC_URL must be an absolute URL` | Google redirect URI `PUBLIC_URL + "/auth/google/callback"`; `SecureCookies` |
| `FRONTEND_URL` | yes | same as `PUBLIC_URL` | same | auth redirects to `FRONTEND_URL + "/auth/callback"` and `"/auth/error"` |
| `ALLOWED_EMAIL_DOMAINS` | no | split on `,`, each trimmed and lowercased, empties dropped. Empty list = any verified Google account | — | auth's domain check |
| `CONFIG_FILE` | no, default `config.toml` | path to `config.toml` | via `LoadSettings` | `SettingsPath` |
| `TEST_DATABASE_URL` | tests only | a **disposable** database | tests skip without it | `pgtest`, `server` tests |

"Required" means set **and non-empty**: `.env.example` ships `JWT_PRIVATE_KEY=""`, which fails with `JWT_PRIVATE_KEY is required` until you run `go run . keygen`.

### `settings.go`: `config.toml`
- **`Settings`** has one struct per table: `Teams`, `Game`, `Auth`, `Live`, `Server`, `Storage`, each field with a `toml` tag.
- **`Duration`** wraps `time.Duration`; its `UnmarshalText` uses `time.ParseDuration`, so durations are strings like `"30s"`, `"2m"`, `"12h"`. A bare number fails (`time: missing unit in duration "60"`).
- **`LoadSettings(path)`**:
  1. `toml.DecodeFile(path, &s)`. A syntax error, a wrong type (`size = "6"`) or a bad duration is returned **alone**, as `read <path>: toml: line N (last key "game.scan_window"): …`. Validation doesn't run.
  2. **Unknown keys:** every key in `md.Undecoded()` becomes `unknown key <key>`. An unknown table `[extra]` with `x = 1` gives both `unknown key extra` and `unknown key extra.x`.
  3. **Missing keys:** `settingKeys()` lists every `[table, key]` from the struct's `toml` tags (by reflection); each one `md.IsDefined` says is absent becomes `missing key <table>.<key>`. This catches keys whose zero value is valid, like `game.scan_grace`.
  4. **`validate()`:** every out-of-range value (a missing key also shows up here, as zero, unless zero is valid).
  5. All of 2–4 are joined as `<path>: <error>\n<error>…`.
- **`SettingsPath()`**: `$CONFIG_FILE`, else `config.toml`.
- **Constants** (not in `config.toml`, because they're bounds, not tunables):
  - `MaxTeamSize = 16`: the most `teams.size` may be. It sizes teamstate's fixed `[16]StackScan` array.
  - `maxNameLength = 32`: migration `0002`'s `CHECK (char_length(name) BETWEEN 1 AND 32)`.

### Every `config.toml` key
| Key | Go type | Shipped value | Validation (error text) | Means |
|---|---|---|---|---|
| `teams.size` | `int` | `6` | 1–16 (`teams.size must be 1-16`) | Players per team: exactly this many to start, no more can join |
| `teams.name_max_length` | `int` | `32` | 1–32 (`… must be 1-32 (the database's limit)`) | Longest team name, in characters |
| `teams.code_attempts` | `int` | `10` | ≥ 1 | Random 6-digit team codes to try per creation |
| `game.scan_window` | `Duration` | `"1m"` | > 0 | Span a pattern's scans must fit in, bottom to top |
| `game.scan_grace` | `Duration` | `"5s"` | ≥ 0 | Tolerance added to the window; also how long a scan stays live beyond it |
| `game.max_pattern_points` | `int` | `6` | 1–`teams.size` (`… each point needs a different member`) | Most waypoints per pattern; capacity of each team's stack |
| `auth.token_ttl` | `Duration` | `"12h"` | > 0 | Access token lifetime; a role change applies at the next sign-in |
| `auth.login_code_ttl` | `Duration` | `"2m"` | > 0 | Time to redeem a sign-in's one-time code |
| `auth.oauth_state_ttl` | `Duration` | `"10m"` | ≥ 1 s (`auth.oauth_state_ttl must be at least 1s`): the state cookie's `Max-Age` is whole seconds | Time a user may spend on Google's screen |
| `auth.login_code_sweep` | `Duration` | `"10m"` | > 0 | How often expired login codes are deleted |
| `live.heartbeat` | `Duration` | `"25s"` | > 0 | Interval between `: ping` comments on each stream |
| `live.subscriber_buffer` | `int` | `16` | ≥ 1 | Events a stream may have unread before it's dropped |
| `server.read_header_timeout` | `Duration` | `"10s"` | > 0 | Time to send request headers |
| `server.read_timeout` | `Duration` | `"30s"` | ≥ `server.read_header_timeout` (`server.read_timeout must be at least server.read_header_timeout`) | Time to send a whole request, body included. Event streams clear it (see `Serve`) |
| `server.idle_timeout` | `Duration` | `"2m"` | > 0 | Idle keep-alive connection lifetime |
| `server.shutdown_timeout` | `Duration` | `"10s"` | > 0 | Wait for in-flight requests on shutdown |
| `server.max_body_bytes` | `int64` | `1048576` | ≥ 1 | Largest request body |
| `storage.write_timeout` | `Duration` | `"5s"` | > 0 | Cap on each write-through to Postgres |
| `storage.instance_lock_check` | `Duration` | `"5s"` | > 0 | How often the backend checks it still holds the single-instance lock; also the timeout of each check |

Where each value is passed in is in `internal/server/README.md`. Use cases never read the file; they get values through their `Rules` or constructor.

### Cases
| Case | Result |
|---|---|
| `config.toml` missing | `read config.toml: open config.toml: no such file or directory`; exit 1 |
| Syntax error, wrong type, bad duration | That one decode error, nothing else from the file |
| Unknown key or table | `unknown key …` for each, plus any validation errors |
| A key missing | `missing key <table>.<key>`, plus `<key> must be …` if zero is out of range |
| `max_pattern_points` 7 with `size` 6 | `game.max_pattern_points must be 1-teams.size: …` |
| `scan_grace` line removed | `missing key game.scan_grace` (zero would be valid, so only the missing-key check catches it) |
| `oauth_state_ttl = "500ms"` | `auth.oauth_state_ttl must be at least 1s` |
| `read_timeout = "1s"` with `read_header_timeout = "10s"` | `server.read_timeout must be at least server.read_header_timeout` |
| Several env and file problems | All of them, one per line, in one error |
| `PUBLIC_URL="localhost:3000"` | `PUBLIC_URL must be an absolute URL` (no host) |
| `PUBLIC_URL="https://labyrinth.example/"` | Accepted as `https://labyrinth.example`; cookies are `Secure` |

### Concurrency
`Config` and `Settings` are built once in `main` and only read afterwards. Nothing in `config` holds state.

### Tests (`settings_test.go`)
| Test | Covers |
|---|---|
| `TestRepositoryConfigLoads` | The committed `backend/config.toml` is valid |
| `TestLoadSettings` | A complete `validTOML` decodes `size`, `scan_window`, `scan_grace`, `max_pattern_points`, `token_ttl`, `subscriber_buffer`, `write_timeout` |
| `TestLoadSettingsRejects` | Each is refused, naming the problem: unknown key, missing `live.heartbeat`, bad duration, negative grace, missing `scan_grace` (`missing key game.scan_grace`), `oauth_state_ttl` of 500 ms, `read_timeout` 1 s under a 10 s header timeout, zero body limit, team of 17, name length 33, 7 pattern points with 6 members, zero window; and one case per other key `validate` can reject, each with its exact message: zero `code_attempts`, `token_ttl`, `login_code_ttl`, `login_code_sweep`, `heartbeat`, `subscriber_buffer`, `read_header_timeout`, `idle_timeout`, `shutdown_timeout`, `write_timeout`, `instance_lock_check`. Each replaced line must appear exactly once in `validTOML` |

Not tested: `Load()` and the environment variables, and the key functions. A missing `scan_grace` is a case in `TestLoadSettingsRejects`.

## config/configtest
- **`Settings(t)`** loads `backend/config.toml` and fails the test if it's invalid.
- It finds the file from its own source path (`runtime.Caller(0)`, four directories up), so it works from any package's test directory and ignores `$CONFIG_FILE`.
- **Why:** tests run with exactly the shipped rules. Tests read values from it (`e.cfg.Settings.Teams.Size`) instead of repeating numbers (AGENTS.md §10).

## httpx
### The envelope
`Response{Success bool; Code string; Message string; Payload any}` with JSON tags `success`, `code,omitempty`, `message,omitempty`, `payload,omitempty`. `payload` is omitted only when it's `nil`; an empty map or slice is still written.

| Function | Status | Body |
|---|---|---|
| `OK(w, payload)` | `200` | `{"success":true,"payload":…}` |
| `Error(w, status, message)` | `status` | `{"success":false,"message":"…"}` |
| `ErrorCode(w, status, code, message)` | `status` | `{"success":false,"code":"…","message":"…"}` |
| `JSON(w, status, body)` | `status` | `body` as is (used for `201` with a `Response`) |

Success, from `GET /healthz`:
```json
{"success":true,"payload":{"streams":3}}
```

Error without a code (the frontend shows the message), from `GET /healthz`:
```json
{"success":false,"message":"database unreachable"}
```

Error with a code (the frontend handles it specifically), from `POST /api/teams/join`:
```json
{"success":false,"code":"team_full","message":"team is full"}
```

Created, from `POST /api/teams` via `JSON(w, 201, Response{Success: true, Payload: t})`:
```json
{"success":true,"payload":{"id":"482913","name":"Evil Exes",…}}
```

**Every `JSON` response:**
- `Content-Type: application/json`
- `Cache-Control: no-store`
- body from `json.Marshal` **before anything is written**, plus a trailing newline
- if encoding fails (e.g. a payload holding a channel), nothing of it is sent: the error is logged through `slog` (`encode response`, using the default logger `main` sets), and the response is a complete `500`:
```json
{"success":false,"message":"internal error"}
```
So a client never sees a truncated body or a `200` with a broken payload.

### `DecodeJSON(w, r, dst)`
- Decodes **one** JSON value from `r.Body` into `dst`, with `DisallowUnknownFields`. `w` is unused.
- Doesn't check `Content-Type`.
- The size cap isn't here: the server wraps every request in `http.MaxBytesHandler` at **1,048,576 bytes** (`server.max_body_bytes`), so reading past it returns `*http.MaxBytesError` from `DecodeJSON`.

| Body | Result |
|---|---|
| `{"shape":"square"}` into `struct{Shape string}` | `nil`; `Shape` set |
| `{"shape":"square","x":1}` | error: `json: unknown field "x"` |
| `{"shape":1}` | error (type mismatch) |
| empty | error `EOF` |
| `null` or `{}` | `nil`; `dst` left zero. Callers check required fields (`req.Shape == ""`), and use pointer fields for required booleans (`Ready *bool`) |
| `{"shape":"square"} trailing` | `nil`: only the first value is read |
| over 1,048,576 bytes | error (`http: request body too large`) |

Each caller maps any error to its own response: `400 invalid_body` in teams, teamstate and `POST /auth/token`.

### Tests (`respond_test.go`)
| Test | Covers |
|---|---|
| `TestJSONEncodeFailureIs500` | A payload with a channel gives `500` and exactly the `internal error` body |
| `TestJSONWritesEnvelope` | `ErrorCode(w, 409, "team_full", …)`: status, `Content-Type`, `Cache-Control` and the body with its newline |

## identity
- **`Identity{UserID uuid.UUID, Email, Name, Role string}`**: who made the request, from their verified token. `Role` is as of sign-in, so a role change shows here only after the next sign-in (at most **12 h**, `auth.token_ttl`).
- **`With(ctx, id)`**: a context carrying `id`, under an unexported key type (no other package can collide with it or forge it).
- **`From(ctx)`**: the `Identity` and whether one was set.
- **`UserID(r)`**: the caller's ID. Only valid behind auth's `RequireAuth`; elsewhere it returns `uuid.Nil` silently.
- **Who uses it:** `RequireAuth` calls `With`. Every authed handler calls `UserID(r)`. Nothing reads `Email`, `Name` or `Role` yet.
- **Why it exists:** `auth` depends on `user`, and every use case needs the caller. Putting the caller type here lets them read it without importing `auth`, which would create a cycle.
- No state, no locks; a context value is immutable.

## live
Server-sent events. Use cases publish full snapshots to users through the `Hub`; `Serve` writes one subscription to one HTTP response.

### Data model
```
Event                                   one server-sent event; 40 bytes on 64-bit
  Name  string                          event name: "team", "no_team", "team_state"
  Data  []byte                          compact JSON, no newlines; shared, never modified

Hub
  mu      sync.Mutex                    guards everything below; one lock for all users
  subs    map[uuid.UUID]                user ID →
            map[*Subscription]struct{}    that user's open streams (a set)
  closed  bool                          set by Close; Subscribe then starts closed
  buffer  int                           live.subscriber_buffer (16)

Subscription                            one open stream
  C   <-chan Event                      what Serve reads; closed when the subscription ends
  ch  chan Event                        the same channel, send side; capacity = buffer
  h   *Hub
  id  uuid.UUID                         the user it belongs to
```
- **Keyed by user, not team,** so a stream follows its user from team to team with nothing to re-subscribe.
- **A set per user,** so every tab gets every event.
- An empty set is deleted from `subs`, so the map holds only users with open streams.
- `Data` is the same slice for every recipient; publishers build it once with `json.Marshal` and never touch it again.

### Functions
- **`NewHub(buffer)`**: an empty hub.
- **`Subscribe(userID)`**: makes a channel of capacity `buffer`, then under `mu` adds it to the user's set. If the hub is closed, closes the channel and returns it unregistered: the caller's `Serve` returns at once. **Subscribe before reading the state you send first** (see `internal/server/README.md`).
- **`Subscription.Close()`**: `remove` → `dropLocked` under `mu`. Safe to call any number of times, and after `Hub.Close`.
- **`dropLocked(sub)`**: if `sub` is still in its user's set, deletes it (and the set, if now empty) and closes its channel. Otherwise a no-op. This check is what makes every close happen exactly once.
- **`Send(userID, ev)`**: under `mu`, for each of the user's subscriptions, a non-blocking send. If the channel is full, `dropLocked`. Never blocks; a user with no streams costs one map lookup.
- **`Count()`**: total open subscriptions across all users (for `/healthz`). Walks every user's set.
- **`Close()`**: sets `closed`, closes every channel, clears `subs`. Called through `srv.RegisterOnShutdown`.

### The slow-subscriber rule
A subscription's channel holds **16** (`live.subscriber_buffer`) events. When `Send` finds it full, i.e. a 17th event arrives while 16 are unread, the subscription is **dropped**:
1. removed from the hub and its channel closed
2. `Serve` still drains the 16 buffered events, writes them, then sees the closed channel and returns, ending the response
3. the browser's `EventSource` reconnects; the new stream starts from fresh snapshots

Nothing is lost, because every event is a full snapshot, never a diff. Only the slow stream is dropped; the user's other tabs and other users are unaffected.

**Why drop instead of block:** `Send` runs while a use case holds a team's write lock. Blocking on one slow phone would stall every write to that team.

### `Serve(w, r, sub, initial, heartbeat)`
1. Sets headers, then `200`:
   - `Content-Type: text/event-stream`
   - `Cache-Control: no-store`
   - `X-Accel-Buffering: no` (nginx: don't buffer)
2. Clears the connection's read deadline (`ResponseController.SetReadDeadline(time.Time{})`). The server's **30 s** (`server.read_timeout`) covers a whole request; a stream lasts hours, and hitting the deadline would cancel its context. A writer without deadline support (httptest's) ignores this.
3. Writes each `initial` event and flushes after each.
4. Loops until one of these, then returns:
   - `r.Context()` done (client disconnected, or the server closed the connection)
   - `sub.C` closed (dropped for being slow, or the hub closed)
   - a write or flush fails
5. Each event from `sub.C` is written and flushed, then the heartbeat ticker is **reset**.
6. A `time.Ticker` at **25 s** (`live.heartbeat`) writes `: ping` and flushes. It starts after the initial events. Because every event resets it, a ping goes out only after 25 s with **no** event: a busy stream gets none.

Flushing uses `http.NewResponseController(w).Flush()`; if the writer can't flush, the first flush returns an error and `Serve` returns.

### Wire format
Each event is exactly `event: <Name>\ndata: <Data>\n\n`. A heartbeat is `: ping\n\n`, an SSE comment that `EventSource` ignores. No `id:` or `retry:` fields are sent, so reconnection uses the browser's default delay and nothing is replayed.

A member of team `482913` connects, nothing happens for 25 s, then a teammate readies. Byte for byte, after the headers (`␤` = `\n`):
```
event: team␤
data: {"id":"482913","name":"Live","leader_id":"6f1c…","status":"LOBBY","created_at":"2026-10-17T12:00:00Z","started_at":null,"members":[…]}␤
␤
event: team_state␤
data: {"team_id":"482913","waypoints":[],"patterns":[],"shapes":[],"selection":null,"stack":[]}␤
␤
: ping␤
␤
event: team␤
data: {"id":"482913",…,"members":[…,{"id":"9a2b…","name":"b","avatar_url":null,"is_ready":true,"joined_at":"2026-10-17T12:00:05Z"}]}␤
␤
```
A user in no team gets `event: no_team␤data: {}␤␤` instead of the first two events.

`Data` must not contain a newline: SSE would split it into two `data:` lines. `json.Marshal` never emits one (newlines inside strings are escaped as `\n`).

### Cases
| Case | Result |
|---|---|
| A user has several tabs open | Each tab's stream gets every event |
| A stream has 16 unread events and another arrives | That stream is dropped after its 16 are written; others unaffected |
| `Send` to a user with no streams | Nothing happens |
| `Subscription.Close` twice, or after `Hub.Close` | No-op |
| `Subscribe` after `Hub.Close` | Returns a closed subscription; not counted |
| `Hub.Close` twice | Second is a no-op |
| Client disconnects | `Serve` returns via `r.Context()`; the caller's deferred `Close` removes the subscription |
| No events for 25 s | `: ping` keeps proxies and mobile networks from closing the connection |
| An event every 10 s | No pings: each event resets the ticker |
| Stream open longer than 30 s (`server.read_timeout`) | Unaffected: `Serve` cleared the read deadline |

### Concurrency
- **One mutex for the whole hub.** `Send`, `Subscribe`, `Close` and `Count` all take it. Held only for map work and non-blocking channel sends, never for I/O.
- **Channels are closed only under `mu`**, and only after removal from `subs`, so `Send` never sends on a closed channel and nothing closes a channel twice.
- **Per-stream order** is the order `Send` was called for that user. Use cases publish while holding the entry lock, so each team's events go out in write order.
- `Serve` runs in the request's goroutine and is the only reader of `sub.C`.

### Tests (`hub_test.go`, `stream_test.go`)
| Test | Covers |
|---|---|
| `TestHubDropsSlowSubscriberOnly` | Two streams for one user; `live.subscriber_buffer` + 1 = 17 sends; the slow one gets exactly **16** (`live.subscriber_buffer`, read with `configtest`) then closes; the fast one keeps receiving; `Count` is 1 |
| `TestHubCloseAndRemove` | `Close` twice is safe; `Hub.Close` closes open streams; `Close` after it is safe; a late `Subscribe` starts closed; `Count` is 0 |
| `TestHubSendsOnlyToThatUser` | An event for one user doesn't reach another |
| `TestHeartbeatOnlyWhenIdle` | With a 150 ms heartbeat: events every 30 ms for 3 heartbeats give no pings; then idle, a ping arrives within 2.5 heartbeats |

`Serve`'s format and disconnect are tested over HTTP in `internal/server/events_test.go`.

## postgres
### `database.go`
- **`DB`** wraps one `*pgxpool.Pool`.
- **`Connect(ctx, url)`**: `pgxpool.New`, then `Ping`. On ping failure, closes the pool and returns the error. Pool size and other pool settings use pgx's defaults, overridable through `DATABASE_URL` parameters (e.g. `pool_max_conns`).
- **`Pool()`**: the pool, for use cases' `db.go` files.
- **`Ping(ctx)`**: used by `/healthz`.
- **`Close()`**: closes the pool.
- **`ErrNotFound`** (`"not found"`): the sentinel every `db.go` returns for a missing row, so use cases don't depend on `pgx.ErrNoRows`. Returned by `user`, `auth` (unknown or expired login code) and `teams`.
- **`SeedDev2025(ctx)`**: runs the embedded `seeds/dev_2025.sql` in one `Exec`. The file wraps everything in `BEGIN … COMMIT` and every insert is `ON CONFLICT DO NOTHING`, so it's idempotent. It inserts 11 waypoints (`1001`–`1011`), 5 shapes (`collinear`, `lightning`, `square`, `tree`, `triangle`) and 359 patterns with their waypoints. It never updates or deletes, so an edited row in the database stays edited. Content is read at startup: run `go run . seed` while the backend is stopped, then start it. `seed` takes the instance lock first, so it refuses (`ErrAnotherInstance`) while a backend serves the same database.

### `migrate.go`: the migration runner
**`Migrate(ctx, log)`**, run on every start (and by `migrate` and `seed`), always **after** `main` has taken the instance lock:
1. Acquires one pool connection for the whole run.
2. `SELECT pg_advisory_lock(7272026)` (`migrationLockID`). **Blocks** until any other migrating process finishes. Cancelling `ctx` (Ctrl-C) aborts the wait.
3. Deferred: `pg_advisory_unlock(7272026)` with a non-cancellable context, so it runs even after Ctrl-C.
4. `CREATE TABLE IF NOT EXISTS schema_migrations`, then `ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum TEXT` (databases migrated before checksums get the column).
5. Reads every applied `version` and its `checksum`.
6. Lists the embedded `migrations/*.sql`, sorted by filename. The 4-digit prefix is what makes string order numeric order.
7. **Checks every file first**, computing its SHA-256 (lowercase hex, 64 characters). For each file, by its version (filename without `.sql`, e.g. `0003_content_progress`):
   - not applied → queued
   - applied, `checksum` NULL (recorded before checksums) → `UPDATE … SET checksum` to the file's hash (backfill)
   - applied, `checksum` differs → returns `migration <version>: migration was edited after it was applied; add a new migration instead` (**`ErrMigrationEdited`**). Nothing queued is applied.
8. For each queued file: one transaction runs the file and inserts its version and checksum. Logs `applied migration version=…`.
9. On a failure, returns `migration <version>: <error>`. That file's transaction rolls back; earlier files stay applied.

| `schema_migrations` column | Type | Meaning |
|---|---|---|
| `version` | `TEXT PRIMARY KEY` | filename without `.sql` |
| `applied_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | when |
| `checksum` | `TEXT` (nullable) | SHA-256 of the file as applied, lowercase hex. NULL only on rows from before checksums, until the next `Migrate` backfills it |

Cases:

| Case | Result |
|---|---|
| Up to date | No-op (`TestMigrationsAreIdempotent`; `setup` in server tests migrates twice) |
| Two processes start together | The second waits on the lock, then finds everything applied |
| A migration fails | Error; the backend doesn't start; fix with a **new** file |
| An applied file is edited | `ErrMigrationEdited`, before any pending file is applied; the backend doesn't start. Undo the edit and add a new file |
| A row from before checksums (`checksum` NULL) | Backfilled with the current file's hash; no error |
| The database has a version the binary doesn't (an older binary) | Ignored |

The migrations:

| File | Creates |
|---|---|
| `0001_users.sql` | `user_role` enum; `userprofile`; `auth_code` (SHA-256 of each login code) and its `expires_at` index |
| `0002_teams.sql` | `team_status` enum; `team` (6-digit `id`, name 1–32 characters); `teammember` (`user_id` unique: one team per user) |
| `0003_content_progress.sql` | content: `waypoint`, `shape`, `pattern`, `pattern_waypoint`; progress: `team_scan`, `team_pattern` |
| `0004_selection_stack.sql` | `team_selection`; index `team_scan_team_id_idx` on `team_scan (team_id, id DESC)` |
| `0005_scan_cleanup.sql` | Drops `team_scan_team_idx` (`team_id`, from 0003), redundant with 0004's index. Adds `COMMENT ON TABLE team_scan` correcting 0003's SQL comment ("every scan, including repeats"): one row per **accepted** scan; rejected scans are never written. Never edit 0003 to fix it: its checksum would change |

Each use case's README documents its tables and constraints.

### `instance_lock.go`: one backend per database
**`AcquireInstanceLock(ctx, check)`** returns an **`*InstanceLock`**:
1. Acquires a pool connection and keeps it.
2. `SELECT pg_try_advisory_lock(7272027)` (`instanceLockID`, different from the migration key). Non-blocking.
3. `false` → releases the connection, returns **`ErrAnotherInstance`** (`another backend instance is already running against this database`).
4. `true` → starts the watcher goroutine and returns the lock.

```
InstanceLock
  conn      *pgxpool.Conn     the session holding the lock
  lost      chan struct{}     closed when a check fails; read through Lost()
  stop      chan struct{}     closed by Release to stop the watcher
  done      chan struct{}     closed when the watcher exits
  stopOnce  sync.Once         makes Release idempotent
```

- **Watcher:** every **5 s** (`storage.instance_lock_check`) it asks, on the lock's own connection and with the same 5 s as timeout, whether `pg_locks` has a granted advisory lock `classid 0, objid 7272027, objsubid 1` for `pg_backend_pid()`. A query error, a timeout or `false` closes `lost` and the watcher stops.
- **`Lost()`**: the channel closed when the lock is gone.
- **`Release()`**: stops the watcher and waits for it, runs `pg_advisory_unlock(7272027)`, returns the connection. Safe to call more than once (`sync.Once`).

**Who takes it:** `main`, right after connecting and **before** `Migrate`, for serving and for the `migrate` and `seed` subcommands. So neither subcommand can touch a database a backend is serving from. `keygen` and `pubkey` don't connect.

**Losing it:** `main` selects on `lock.Lost()` alongside the signal context. When it closes, it logs `instance lock lost; shutting down`, shuts the server down as on SIGTERM, and returns **`ErrInstanceLockLost`** (`lost the instance lock`), so the process exits 1.

- The lock is **session-level**: held as long as that connection lives. If the process dies, Postgres ends the session and frees the lock.
- It costs one pool connection for the process's lifetime.
- **Why:** team and game state live in one process's memory, written through to Postgres. A second process would serve its own diverging copy.

| Case | Result |
|---|---|
| First backend | Gets the lock |
| Second backend, same database | `ErrAnotherInstance`; exit 1, before migrating |
| `go run . migrate` or `go run . seed` while a backend serves | `ErrAnotherInstance`; exit 1, nothing migrated or seeded |
| First stops cleanly | `Release` unlocks; a new one can start |
| First is killed | Lock freed when Postgres notices the closed connection |
| The lock's connection dies while serving (Postgres restart, `pg_terminate_backend`) | At the next check (every **5 s**, `storage.instance_lock_check`; up to 10 s if that check's query hangs until its 5 s timeout), `Lost()` closes; the backend shuts down and exits 1 with `ErrInstanceLockLost` |
| Two backends on **different databases** (or different schemas of one database) | Advisory locks are per database: different databases don't conflict; different schemas of one database **do** |

### Tests (`lock_test.go`, schema `postgres_test`)
| Test | Covers |
|---|---|
| `TestOnlyOneInstance` | A second pool can't take the lock; `Release` twice is safe; after it the second can |
| `TestLostInstanceLockIsNoticed` | With a 20 ms check, `pg_terminate_backend` on the lock's session closes `Lost()` within 50 checks; another instance can then take the lock |
| `TestEditedMigrationIsRefused` | A NULL checksum is backfilled with a 64-character hash; a changed checksum gives `ErrMigrationEdited` |
| `TestMigrationsAreIdempotent` | Migrating an up-to-date schema is a no-op |

Not tested: a failing migration and `SeedDev2025` (exercised by teamstate's and server's tests).

## postgres/pgtest
- **`URL(t, schema)`**:
  1. Skips the test if `TEST_DATABASE_URL` is unset.
  2. Connects to it and runs `DROP SCHEMA IF EXISTS <schema> CASCADE; CREATE SCHEMA <schema>` (name quoted with `pgx.Identifier`).
  3. Returns `TEST_DATABASE_URL` with `search_path=<schema>` added to its query, so every connection from it works inside that schema.
- **`Open(t, url)`**: `postgres.Connect` and `Migrate` (with `Quiet`), failing the test on error; the pool closes at test cleanup.
- **`Quiet`**: a `slog.Logger` that discards everything.
- **`Must(v, err)`**: returns `v`, or panics on `err` (Go can't pass a two-result call alongside `t`).

Schemas in use:

| Package | Schema |
|---|---|
| `platform/postgres` | `postgres_test` |
| `auth` | `auth_test` |
| `user` | `user_test` |
| `teams` | `teams_test` |
| `teamstate` | `teamstate_test` |
| `server` | `public` (its own `setup`, not `pgtest`) |

**Why a schema per package:** `go test ./...` runs packages in parallel. Each gets its own tables, types and `schema_migrations`, so they can't see each other's rows. Within a package, tests run one at a time and each call to `URL` wipes the schema again.

**Why `TEST_DATABASE_URL` must be disposable:**
- `URL` drops schemas with `CASCADE`, and the server tests drop `public`, which is where a real database's tables are.
- Advisory locks are per database, not per schema. A real backend running on that database holds `7272027`, so `TestOnlyOneInstance` would fail; and tests migrating would wait on a real backend's migration.

Use the throwaway container from AGENTS.md §14.

## Not decided yet
- **A lost instance lock is noticed only at the next check.** For up to one check interval, **5 s** (`storage.instance_lock_check`), plus the check's own timeout (another 5 s) after the lock's connection drops, a second backend could start while this one still serves. Lower the interval to narrow the window.
