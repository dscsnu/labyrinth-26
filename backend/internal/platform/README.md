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
| `config` | `config.go`, `settings.go` | Reads `.env` variables and `config.toml`, validates both, reports every problem at once. Parses, generates and prints the Ed25519 signing key and the QR signing secret. | `main`, `server`, `auth`, `teamstate` (`MaxTeamSize`) |
| `config/configtest` | `configtest.go` | Loads the repository's `config.toml` for tests. | tests |
| `httpx` | `respond.go` | The response envelope `{"success", "code", "message", "payload"}` and `DecodeJSON`. | every `api.go`, `server` |
| `identity` | `identity.go` | Carries the signed-in user through a request, so use cases don't import `auth`. | `auth` (sets), everyone else (reads) |
| `live` | `event.go`, `hub.go`, `stream.go` | `Event`: one event, encoded once. `Hub`: fans events out to one user's streams or to all. `Streamer`: writes one stream as server-sent events. | `server`, `teams`, `teamstate` |
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
- **`Config`**: `Port`, `DatabaseURL`, `JWTKey ed25519.PrivateKey`, `QRSecret []byte`, `GoogleClientID`, `GoogleClientSecret`, `PublicURL *url.URL`, `FrontendURL *url.URL`, `AllowedEmailDomains []string`, `Settings Settings`.
- **`Load()`** reads every variable below, then `LoadSettings(SettingsPath())`. Each problem is appended to a list; at the end, if any, it returns `nil` and `errors.Join` of all of them, so one start shows everything that's wrong.
- **`SecureCookies()`**: `true` when `PUBLIC_URL`'s scheme is `https`. Auth uses it for the `Secure` flag on its cookies.
- **`ParsePrivateKey(b64)`**: trims spaces, base64-decodes (standard alphabet), parses PKCS#8 DER, requires Ed25519. Errors:
  - `must be base64 (generate with: go run . keygen)`
  - `not a PKCS#8 private key: …`
  - `must be an Ed25519 key`
- **`ParseQRSecret(b64)`**: trims spaces, base64-decodes, requires at least 32 bytes. Errors:
  - `must be base64 (generate with: go run . keygen)`
  - `must be at least 32 bytes, got N`
- **`GenerateKeyPair()`**: a new Ed25519 pair as base64 PKCS#8 (private, 48 bytes DER → 64 characters) and base64 SPKI (public, 44 bytes DER → 60 characters). Used by `labyrinth keygen`.
- **`GenerateQRSecret()`**: 32 random bytes, base64. Used by `labyrinth keygen`.
- **`PublicKeyBase64(key)`**: the base64 SPKI of `key`'s public half. Used by `labyrinth pubkey`.
- **`getenv(key, fallback)`**: the variable, or `fallback` if unset **or empty**.

### Environment variables
| Variable | Required | Rule | Error if broken | Used by |
|---|---|---|---|---|
| `PORT` | no, default `3000` | not validated | — | `main`: listen on `":" + PORT` |
| `DATABASE_URL` | yes | non-empty; its format is checked only when `postgres.Connect` parses it | `DATABASE_URL is required` | `main` |
| `JWT_PRIVATE_KEY` | yes | `ParsePrivateKey` | `JWT_PRIVATE_KEY is required`, or `JWT_PRIVATE_KEY: <parse error>` | `auth.NewTokens` |
| `QR_SECRET` | yes | `ParseQRSecret` | `QR_SECRET is required`, or `QR_SECRET: <parse error>` | `qr.New` |
| `GOOGLE_CLIENT_ID` | yes | non-empty | `GOOGLE_CLIENT_ID is required` | `auth.NewGoogle` |
| `GOOGLE_CLIENT_SECRET` | yes | non-empty | `GOOGLE_CLIENT_SECRET is required` | `auth.NewGoogle` |
| `PUBLIC_URL` | yes | trailing `/` trimmed; must parse with a scheme and host | `PUBLIC_URL is required` / `PUBLIC_URL must be an absolute URL` | Google redirect URI `PUBLIC_URL + "/auth/google/callback"`; `SecureCookies` |
| `FRONTEND_URL` | yes | same as `PUBLIC_URL` | same | auth redirects to `FRONTEND_URL + "/auth/callback"` and `"/auth/error"` |
| `ALLOWED_EMAIL_DOMAINS` | no | split on `,`, each trimmed and lowercased, empties dropped. Empty list = any verified Google account | — | auth's domain check |
| `CONFIG_FILE` | no, default `config.toml` | path to `config.toml` | via `LoadSettings` | `SettingsPath` |
| `TEST_DATABASE_URL` | tests only | a **disposable** database | tests skip without it | `pgtest`, `server` tests |

"Required" means set **and non-empty**: `.env.example` ships `JWT_PRIVATE_KEY=""`, which fails with `JWT_PRIVATE_KEY is required` until you run `go run . keygen`.

### `settings.go`: `config.toml`
- **`Settings`** has one struct per table: `Teams`, `Game`, `QR`, `Auth`, `Live`, `Server`, `Storage`, each field with a `toml` tag.
- **`Duration`** wraps `time.Duration`; its `UnmarshalText` uses `time.ParseDuration`, so durations are strings like `"30s"`, `"2m"`, `"12h"`. A bare number fails (`time: missing unit in duration "60"`).
- **`LoadSettings(path)`**:
  1. `toml.DecodeFile(path, &s)`. A syntax error, a wrong type (`size = "6"`) or a bad duration is returned **alone**, as `read <path>: toml: line N (last key "live.heartbeat"): …`. Validation doesn't run.
  2. **Unknown keys:** every key in `md.Undecoded()` becomes `unknown key <key>`. An unknown table `[extra]` with `x = 1` gives both `unknown key extra` and `unknown key extra.x`.
  3. **Missing keys:** `settingKeys()` lists every `[table, key]` from the struct's `toml` tags (by reflection); each one `md.IsDefined` says is absent becomes `missing key <table>.<key>`. This catches keys whose zero value is valid, like `spell.per_100m`.
  4. **`validate()`:** every out-of-range value (a missing key also shows up here, as zero, unless zero is valid).
  5. All of 2–4 are joined as `<path>: <error>\n<error>…`.
- **`SettingsPath()`**: `$CONFIG_FILE`, else `config.toml`.
- **Constants** (not in `config.toml`, because they're bounds, not tunables):
  - `MaxTeamSize = 16`: the most `teams.size` may be. It sizes teamstate's fixed `[16]StackScan` array.
  - `maxNameLength = 32`: migration `0002`'s `CHECK (char_length(name) BETWEEN 1 AND 32)`.

### Every `config.toml` key
| Key | Go type | Shipped value | Validation (error text) | Means |
|---|---|---|---|---|
| `teams.size` | `int` | `6` | 1–16 (`teams.size must be 1-16`) | Most players per team: no more can join; any number from 1 can start |
| `teams.name_max_length` | `int` | `32` | 1–32 (`… must be 1-32 (the database's limit)`) | Longest team name, in characters |
| `teams.code_attempts` | `int` | `10` | ≥ 1 | Random 6-digit team codes to try per creation |
| `game.max_pattern_points` | `int` | `6` | 1–15 (`game.max_pattern_points must be 1-15`; `config.MaxPatternPoints`, the size of the stack's fixed array) | Most waypoints per pattern; a team's stack holds this many plus one wrong scan. Not tied to `teams.size`: one member may scan several points |
| `game.bosses` | `int` | `7` | ≥ 1 (`game.bosses must be at least 1`) | How many bosses every team fights, in serial order; the `boss` table must hold exactly serials 1 to this (or be empty), or startup is refused (`bosses.Load`) |
| `game.health` | `int` | `10` | ≥ 1 (`game.health must be at least 1`) | Every team's starting health, and what an admin's revive restores; each boss attack takes its `attack_damage` off it; at 0 the team is knocked out (`teamstate.Rules.Health`) |
| `qr.rotate` | `Duration` | `"5s"` | > 0 (`qr.rotate must be positive`) | How often a waypoint's rotating QR changes; the `/qr` display re-issues a token this often |
| `qr.tolerance` | `Duration` | `"5s"` | > 0 (`qr.tolerance must be positive`) | How much longer than `qr.rotate` a scan stays accepted; a token is valid `qr.rotate` + `qr.tolerance` after issue |
| `auth.token_ttl` | `Duration` | `"12h"` | > 0 | Access token lifetime; a role change applies at the next sign-in |
| `auth.login_code_ttl` | `Duration` | `"2m"` | > 0 | Time to redeem a sign-in's one-time code |
| `auth.oauth_state_ttl` | `Duration` | `"10m"` | ≥ 1 s (`auth.oauth_state_ttl must be at least 1s`): the state cookie's `Max-Age` is whole seconds | Time a user may spend on Google's screen |
| `auth.login_code_sweep` | `Duration` | `"10m"` | > 0 | How often expired login codes are deleted |
| `live.heartbeat` | `Duration` | `"25s"` | > 0 | Interval between `: ping` comments on each stream |
| `live.subscriber_buffer` | `int` | `16` | ≥ 1 | Events a stream may have unread before it's dropped |
| `live.hub_shards` | `int` | `64` | ≥ 1 | Locks the hub spreads streams over, by user ID |
| `live.write_timeout` | `Duration` | `"10s"` | > 0 | Longest one write to a stream may take before the client is cut off |
| `live.retry_min` | `Duration` | `"1s"` | ≥ 1 ms | Shortest reconnect delay sent to a client |
| `live.retry_max` | `Duration` | `"6s"` | ≥ `retry_min` | Longest reconnect delay sent to a client |
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
| `max_pattern_points = 16` | `game.max_pattern_points must be 1-15` |
| `bosses = 0` | `game.bosses must be at least 1` |
| `health = 0` | `game.health must be at least 1` |
| `per_100m` line removed | `missing key spell.per_100m` (zero would be valid, so only the missing-key check catches it) |
| `oauth_state_ttl = "500ms"` | `auth.oauth_state_ttl must be at least 1s` |
| `read_timeout = "1s"` with `read_header_timeout = "10s"` | `server.read_timeout must be at least server.read_header_timeout` |
| Several env and file problems | All of them, one per line, in one error |
| `PUBLIC_URL="localhost:3000"` | `PUBLIC_URL must be an absolute URL` (no host) |
| `PUBLIC_URL="https://labyrinth.example/"` | Accepted as `https://labyrinth.example`; cookies are `Secure` |

### Concurrency
`Config` and `Settings` are built once in `main` and only read afterwards. Nothing in `config` holds state.

### Tests (`config/tests/settings_test.go`)
| Test | Covers |
|---|---|
| `TestRepositoryConfigLoads` | The committed `backend/config.toml` is valid |
| `TestLoadSettings` | A complete `validTOML` decodes `size`, `max_pattern_points`, `bosses`, `health`, `qr.rotate`, `qr.tolerance`, `per_point`, `per_100m`, `walkability`, `token_ttl`, `subscriber_buffer`, `write_timeout` |
| `TestLoadSettingsRejects` | Each is refused, naming the problem: unknown key, missing `live.heartbeat`, bad duration (`heartbeat`), missing `per_100m` (`missing key spell.per_100m`), `oauth_state_ttl` of 500 ms, `read_timeout` 1 s under a 10 s header timeout, zero body limit, team of 17, name length 33, 16 pattern points (over the stack's array), zero bosses, zero health, zero `per_point`, negative `per_100m`, walkability under 1.0; and one case per other key `validate` can reject, each with its exact message: zero `code_attempts`, `token_ttl`, `login_code_ttl`, `login_code_sweep`, `qr.rotate`, `qr.tolerance`, `heartbeat`, `subscriber_buffer`, `hub_shards`, live `write_timeout`, `retry_min`, a `retry_max` below `retry_min`, `read_header_timeout`, `idle_timeout`, `shutdown_timeout`, `write_timeout`, `instance_lock_check`. Each replaced line must appear exactly once in `validTOML` |

Not tested: `Load()` and the environment variables, and the key functions. A missing `per_100m` is a case in `TestLoadSettingsRejects`.

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

### Tests (`httpx/tests/respond_test.go`)
| Test | Covers |
|---|---|
| `TestJSONEncodeFailureIs500` | A payload with a channel gives `500` and exactly the `internal error` body |
| `TestJSONWritesEnvelope` | `ErrorCode(w, 409, "team_full", …)`: status, `Content-Type`, `Cache-Control` and the body with its newline |

## identity
- **`Identity{UserID uuid.UUID, Email, Name, Role string}`**: who made the request, from their verified token. `Role` is as of sign-in, so a role change shows here only after the next sign-in (at most **12 h**, `auth.token_ttl`).
- **`With(ctx, id)`**: a context carrying `id`, under an unexported key type (no other package can collide with it or forge it).
- **`From(ctx)`**: the `Identity` and whether one was set.
- **`UserID(r)`**: the caller's ID. Only valid behind auth's `RequireAuth`; elsewhere it returns `uuid.Nil` silently.
- **Who uses it:** `RequireAuth` and `RequireStreamAuth` call `With`. Every authed handler calls `UserID(r)`. Nothing reads `Email`, `Name` or `Role` yet.
- **Why it exists:** `auth` depends on `user`, and every use case needs the caller. Putting the caller type here lets them read it without importing `auth`, which would create a cycle.
- No state, no locks; a context value is immutable.

## live
Server-sent events. Use cases publish full snapshots through the `Hub`, to one user (`Send`) or to every stream (`Broadcast`); a `Streamer` writes one subscription to one HTTP response.

### Data model
```
Event                                   one server-sent event, encoded once; immutable
  name   string                         event name, the resource: "hello", "team", "no_team", "team_state"
  typ    EventType                      why it was sent: "spell_cast", "member_joined", "snapshot", …
  data   []byte                         the envelope {"type","message"} as compact JSON; shared, never modified
  frame  []byte                         the wire bytes "event: …\ndata: …\n\n", built by NewEvent

EventType string                        the envelope's "type"; Snapshot and Resync are declared here,
                                        the causes by the use case that publishes them

Version                                 orders one resource's snapshots (team, team_state)
  Epoch  int64                          the backend process's start time, Unix ms (one per process)
  Seq    uint64                         changes since then; Next(epoch) is the following version

Hub
  shards  []shard                       live.hub_shards (64); a user's streams all live in one shard
  seed    maphash.Seed                  picks the shard: maphash.Comparable(seed, userID) % len(shards)
  buffer  int                           live.subscriber_buffer (16)

shard
  mu      sync.Mutex                    guards everything below, for this shard's users only
  subs    map[uuid.UUID]                user ID →
            map[*Subscription]struct{}    that user's open streams (a set)
  closed  bool                          set by Hub.Close; Subscribe then starts closed

Subscription                            one open stream
  C      <-chan Event                   what Serve reads; closed when the subscription ends
  ch     chan Event                     the same channel, send side; capacity = buffer
  shard  *shard                         where it's registered
  id     uuid.UUID                      the user it belongs to

Streamer                                built once in server.Build, shared by every stream
  Heartbeat           time.Duration     live.heartbeat (25 s)
  WriteTimeout        time.Duration     live.write_timeout (10 s)
  RetryMin, RetryMax  time.Duration     live.retry_min, live.retry_max (1 s, 6 s)
```
- **Keyed by user, not team,** so a stream follows its user from team to team with nothing to re-subscribe. Who gets an event is decided by the publisher when it sends: one user, a team's members, or everyone (see `internal/server/README.md`, "Scopes").
- **A set per user,** so every tab gets every event.
- **Sharded,** so publishers for different users rarely wait on each other, and a broadcast never holds the whole hub.
- An empty set is deleted from `subs`, so each map holds only users with open streams.
- **An `Event` is encoded once.** `frame` is shared by every stream it's sent to; `Serve` only copies bytes. Its fields are unexported so it can't be changed after `NewEvent`; read them with `Name()`, `Type()` and `Data()`.
- **Every event's data is the envelope** `{"type": …, "message": …}`: `type` is why it was sent, `message` the resource's full state. The format and every type are in `internal/server/README.md`, "Event format".
- **`Version` lives here** because both teams and teamstate put it in their payloads and neither may import the other (AGENTS.md §4). Its doc comment states the client rule: keep a copy if its epoch differs, or the epoch matches and seq is higher.

### Functions
- **`NewEvent(name, typ, message)`**: encodes the envelope `{"type": typ, "message": message}` with `encoding/json` (a `json.RawMessage` is passed through, compacted), once, and builds the frame. JSON from `encoding/json` is compact, so it's always one `data:` line. Fails only if `message` can't be encoded. The zero `Event` has an empty frame, which `Serve` skips.
- **`MustNewEvent(name, typ, message)`**: `NewEvent` for a constant message known to encode (`no_team`'s `{}`, `hello`); panics otherwise.
- **`Snapshot`, `Resync`**: the two `EventType`s every resource uses: the state a stream opens with, and the state reloaded after a failed write.
- **`Version.Next(epoch)`**: the version after this one, in `epoch`.
- **`NewHub(shards, buffer)`**: an empty hub with `shards` shards (panics below 1: a programming error, since config validation already refuses it).
- **`Subscribe(userID)`**: makes a channel of capacity `buffer`, then under the user's shard lock adds it to the user's set. If the hub is closed, closes the channel and returns it unregistered: the caller's `Serve` returns at once. **Subscribe before reading the state you send first** (see `internal/server/README.md`).
- **`Subscription.Close()`**: `dropLocked` under its shard's lock. Safe to call any number of times, and after `Hub.Close`.
- **`dropLocked(sub)`**: if `sub` is still in its user's set, deletes it (and the set, if now empty) and closes its channel. Otherwise a no-op. This check is what makes every close happen exactly once.
- **`Send(userID, ev)`**: under the user's shard lock, for each of the user's subscriptions, a non-blocking send (`deliverLocked`). If the channel is full, `dropLocked`. Never blocks; a user with no streams costs one hash and one map lookup.
- **`Broadcast(ev)`**: for each shard in turn, under that shard's lock, the same non-blocking send to every subscription. Never blocks, and never holds more than one shard: Sends to other shards carry on meanwhile.
- **`Count()`**: total open subscriptions (for `/healthz`), summing each shard under its lock.
- **`Close()`**: per shard, sets `closed`, closes every channel, clears `subs`. Called through `srv.RegisterOnShutdown`.

### The slow-subscriber rule
A subscription's channel holds **16** (`live.subscriber_buffer`) events. When `Send` or `Broadcast` finds it full, i.e. a 17th event arrives while 16 are unread, the subscription is **dropped**:
1. removed from the hub and its channel closed
2. `Serve` still drains the 16 buffered events, writes them, then sees the closed channel and returns, ending the response
3. the browser's `EventSource` reconnects after its `retry:` delay; the new stream starts from fresh snapshots

Nothing is lost, because every event is a full snapshot, never a diff. Only the slow stream is dropped; the user's other tabs and other users are unaffected.

**Why drop instead of block:** `Send` runs while a use case holds a team's write lock. Blocking on one slow phone would stall every write to that team.

### `Streamer.Serve(w, r, sub, initial)`
1. Sets headers, then `200`:
   - `Content-Type: text/event-stream`
   - `Cache-Control: no-store`
   - `X-Accel-Buffering: no` (nginx: don't buffer)
2. Clears the connection's read deadline (`ResponseController.SetReadDeadline(time.Time{})`). The server's **30 s** (`server.read_timeout`) covers a whole request; a stream lasts hours, and hitting the deadline would cancel its context. A writer without deadline support (`http.ErrNotSupported`, httptest's) is fine.
3. Writes `retry: <ms>`, a delay picked uniformly between **1 s** and **6 s** (`live.retry_min`, `live.retry_max`) for this stream, then each `initial` event, and flushes once.
4. Loops until one of these, then returns:
   - `r.Context()` done (client disconnected, or the server closed the connection)
   - `sub.C` closed (dropped for being slow, or the hub closed)
   - a write or flush fails, or a write misses its deadline
5. Each event from `sub.C` is written, then **up to a buffer's worth of events already queued** are written after it (`drain`), then one flush; then the heartbeat ticker is **reset**. A burst costs one flush, and a stream that's always busy still flushes at least every 16 events.
6. A `time.Ticker` at **25 s** (`live.heartbeat`) writes `: ping` and flushes. Because every event resets it, a ping goes out only after 25 s with **no** event: a busy stream gets none.
7. **Every write gets a fresh deadline** of **10 s** (`live.write_timeout`) through `ResponseController.SetWriteDeadline`. A phone that stopped reading without closing the connection fills its TCP window; the write then fails at the deadline and `Serve` returns, freeing its goroutine and buffers instead of waiting minutes for TCP to give up.

`conn` holds the writer, its `ResponseController` and the first error, so a batch of writes is checked once, at its flush.

### Wire format
Each event is exactly `event: <name>\ndata: <envelope>\n\n`, the envelope on one line. A heartbeat is `: ping\n\n`, an SSE comment that `EventSource` ignores. The stream opens with `retry: <ms>\n\n`. No `id:` fields are sent, so nothing is replayed.

A member of team `482913` connects, nothing happens for 25 s, then a teammate readies. Byte for byte, after the headers (`␤` = `\n`):
```
retry: 3412␤
␤
event: hello␤
data: {"type":"connected","message":{"server_time":"2026-10-17T12:00:00.123456Z"}}␤
␤
event: team␤
data: {"type":"snapshot","message":{"id":"482913","name":"Live","leader_id":"6f1c…","status":"LOBBY","created_at":"2026-10-17T12:00:00Z","started_at":null,"members":[…],"version":{"epoch":1760702400000,"seq":2}}}␤
␤
event: team_state␤
data: {"type":"snapshot","message":{"team_id":"482913","waypoints":[],"patterns":[],"shapes":[],"selection":null,"last_cast":null,"stack":[],"poisoned":false,"cast_count":0,"version":{"epoch":1760702400000,"seq":0}}}␤
␤
: ping␤
␤
event: team␤
data: {"type":"ready_changed","message":{"id":"482913",…,"members":[…,{"id":"9a2b…","name":"b","avatar_url":null,"is_ready":true,"joined_at":"2026-10-17T12:00:05Z"}],"version":{"epoch":1760702400000,"seq":3}}}␤
␤
```
A user in no team gets `event: no_team␤data: {"type":"snapshot","message":{}}␤␤` instead of `team` and `team_state`. Every event, what triggers it and the ordering guarantees: `internal/server/README.md`, "The event stream".

### Cases
| Case | Result |
|---|---|
| A user has several tabs open | Each tab's stream gets every event |
| A stream has 16 unread events and another arrives | That stream is dropped after its 16 are written; others unaffected |
| `Send` to a user with no streams | Nothing happens |
| `Broadcast` with a slow stream among many | Only the slow one is dropped |
| `Subscription.Close` twice, or after `Hub.Close` | No-op |
| `Subscribe` after `Hub.Close` | Returns a closed subscription; not counted |
| `Broadcast` or `Close` after `Hub.Close` | No-op |
| Client disconnects | `Serve` returns via `r.Context()`; the caller's deferred `Close` removes the subscription |
| Client stops reading but keeps the connection | The next write fails after **10 s** (`live.write_timeout`); `Serve` returns |
| Several events queued at once | Written together, one flush |
| No events for 25 s | `: ping` keeps proxies and mobile networks from closing the connection |
| An event every 10 s | No pings: each event resets the ticker |
| Stream open longer than 30 s (`server.read_timeout`) | Unaffected: `Serve` cleared the read deadline |
| The backend restarts | Every client reconnects after its own `retry:` delay, spread over 1–6 s |

### Concurrency
- **One mutex per shard.** `Send` and `Subscribe` take their user's shard; `Broadcast`, `Count` and `Close` take each shard in turn, never two at once. Held only for map work and non-blocking channel sends, never for I/O.
- **Channels are closed only under their shard's lock**, and only after removal from `subs`, so nothing sends on a closed channel and nothing closes a channel twice.
- **Per-stream order** is the order events were queued for that stream. Use cases publish while holding the entry lock, so each team's events go out in write order. Two `Broadcast`s from different publishers may reach different streams in different orders: a game-wide publisher broadcasts under its own entry lock.
- `Serve` runs in the request's goroutine and is the only reader of `sub.C`.

### Performance
Measured on an Apple M-series laptop (10 cores), `go test -bench`, before and after the sharded hub and pre-encoded frames:

| Benchmark | Before | After |
|---|---|---|
| `BenchmarkHubSend`: parallel `Send`, 10,000 users | 600 ns/op | 135 ns/op |
| `BenchmarkTeamStatePublish` (`teamstate/tests`): parallel `team_state` publishes over 1,666 teams (10,000 players) | 4.4 µs/op | 1.0 µs/op |
| `BenchmarkHubBroadcast`: queue one event on 10,000 streams | 3.2 ms (a `Send` per user, no `Broadcast` existed) | 3.9 ms, holding one shard at a time |
| `BenchmarkBroadcastFanout`: 10,000 real HTTP streams, broadcast → last client read | ~135 ms; p50 33 ms, p99 178 ms | ~130 ms; p50 35 ms, p99 180–240 ms |

The fan-out benchmark runs the 10,000 clients on the same machine as the server, so client parsing competes with it for CPU; the end-to-end figures are dominated by the kernel and the clients, not by the hub. Run it with `go test -run '^$' -bench Fanout -fanout.streams 10000 ./internal/platform/live/tests/` (it opens 20,000 sockets; raise `ulimit -n`).

### Tests (`live/tests/hub_test.go`, `live/tests/stream_test.go`, `live/tests/fanout_test.go`)
| Test | Covers |
|---|---|
| `TestHubDropsSlowSubscriberOnly` | Two streams for one user; `live.subscriber_buffer` + 1 = 17 sends; the slow one gets exactly **16** (`live.subscriber_buffer`, read with `configtest`) then closes; the fast one keeps receiving; `Count` is 1 |
| `TestHubCloseAndRemove` | `Close` twice is safe; `Hub.Close` closes open streams; `Close` and `Broadcast` after it are safe; a late `Subscribe` starts closed; `Count` is 0 |
| `TestHubSendsOnlyToThatUser` | An event for one user doesn't reach another |
| `TestHubBroadcastReachesEveryStream` | 200 users with two streams each, across every shard, get every broadcast; a slow stream gets exactly 16 then is dropped; `Count` is right |
| `TestHubConcurrentUse` | `Subscribe`, `Send`, `Broadcast` and `Close` side by side (meaningful under `-race`) |
| `TestServeWritesFrames` | Exact bytes: the `retry:` line, then each event as one `event:` and one `data:` line holding the envelope, for a struct, a `json.RawMessage` (compacted) and an empty-object message |
| `TestNewEvent` | `Name`, `Type` and `Data` (the envelope); a message that can't be encoded is an error |
| `TestVersionNext` | `Next` counts up from 1 in its epoch |
| `TestServeRetryWithinRange` | 20 streams with a 1–3 s range each send a delay within it |
| `TestServeCoalescesQueuedEvents` | Three events queued before `Serve` starts go out in one flush, in order |
| `TestServeStalledClientTimesOut` | A client that never accepts a write ends `Serve` at the 50 ms write deadline |
| `TestHeartbeatOnlyWhenIdle` | With a 150 ms heartbeat: events every 30 ms for 3 heartbeats give no pings; then idle, a ping arrives within 2.5 heartbeats |
| `BenchmarkHubSend`, `BenchmarkHubBroadcast`, `BenchmarkBroadcastFanout` | See [Performance](#performance). The fan-out benchmark skips under `-short` |

`Serve`'s format, `hello`, cookie auth and disconnect are also tested over HTTP in `internal/server/tests/events_test.go`.

## postgres
### `database.go`
- **`DB`** wraps one `*pgxpool.Pool`.
- **`Connect(ctx, url)`**: `pgxpool.New`, then `Ping`. On ping failure, closes the pool and returns the error. Pool size and other pool settings use pgx's defaults, overridable through `DATABASE_URL` parameters (e.g. `pool_max_conns`).
- **`Pool()`**: the pool, for use cases' `db.go` files.
- **`Ping(ctx)`**: used by `/healthz`.
- **`Close()`**: closes the pool.
- **`ErrNotFound`** (`"not found"`): the sentinel every `db.go` returns for a missing row, so use cases don't depend on `pgx.ErrNoRows`. Returned by `user`, `auth` (unknown or expired login code) and `teams`.
- **`SeedDev2025(ctx)`**: runs the embedded `seeds/dev_2025.sql` in one `Exec`. The file wraps everything in `BEGIN … COMMIT` and every insert is `ON CONFLICT DO NOTHING`, so it's idempotent. It inserts 11 waypoints (`1001`–`1011`), 5 shapes (`collinear`, `lightning`, `square`, `tree`, `triangle`) and 359 patterns with their waypoints. It never updates or deletes, so an edited row in the database stays edited. Then it runs `seeds/dev_2025_spells.sql` (each pattern's `damage`, generated by `cmd/spells`) and `seeds/dev_2025_bosses.sql` (7 placeholder bosses, serials 1–7, HP 200 to 1000, attacking every 300 s for 1 down to every 120 s for 2; `ON CONFLICT (serial) DO UPDATE`, so re-seeding resets their name, asset, description, HP and attacks). Content is read at startup: run `go run . seed` while the backend is stopped, then start it. `seed` takes the instance lock first, so it refuses (`ErrAnotherInstance`) while a backend serves the same database.

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
| `0006_wrong_scans.sql` | Adds `team_scan.wrong BOOLEAN NOT NULL DEFAULT false`: a scan that didn't fit the selected shape, so a restart rebuilds a poisoned attempt as poisoned |
| `0007_team_cast.sql` | `team_cast` (`id` identity PK, `team_id` → `team` `ON DELETE CASCADE`, `pattern_id` → `pattern`, `cast_by` → `userprofile`, `cast_at`, `already_completed`): every cast, repeats included; the newest row per team is `last_cast` after a restart. Index `team_cast_team_id_idx` on `(team_id, id DESC)` |
| `0008_pattern_damage.sql` | `pattern.damage INTEGER CHECK (damage > 0)`: each pattern's precomputed spell damage, loaded by the atlas (written by `seeds/dev_2025_spells.sql`, generated by `cmd/spells`) |
| `0009_bosses.sql` | `boss` (`serial` PK ≥ 1, `name` 1–64 chars, `asset` non-empty, `description`, `hp` > 0): the boss sequence, read by the bosses use case. `team_boss` (`team_id` PK → `team` `ON DELETE CASCADE`, `defeated` ≥ 0, `damage` ≥ 0, `multiplier` > 0, defaults 0, 0, 1): each team's boss progress, written by teamstate with each cast. `team_cast` gains `dealt` (≥ 0, default 0), `boss_serial` (nullable → `boss`) and `defeated_boss` (default false): what each cast did to the bosses |
| `0010_boss_attacks.sql` | `boss` gains `attack_interval_s` (> 0) and `attack_damage` (≥ 0), added with defaults 300 and 1 to fill existing rows, then the defaults dropped so new bosses must give both: every `attack_interval_s` of a team's phase, the boss takes `attack_damage` off the team's health. `team_boss` gains `phase_health` (nullable, ≥ 0; `NULL` = full `game.health`) and `phase_started_at` (nullable; `NULL` = the team's `started_at`): where the current boss phase began. Health itself isn't stored: teamstate derives it from the phase and the clock, so attacks write nothing |

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

### Tests (`postgres/tests/lock_test.go`, schema `postgres_test`)
| Test | Covers |
|---|---|
| `TestOnlyOneInstance` | A second pool can't take the lock; `Release` twice is safe; after it the second can |
| `TestLostInstanceLockIsNoticed` | With a 20 ms check, `pg_terminate_backend` on the lock's session closes `Lost()` within 50 checks; another instance can then take the lock |
| `TestEditedMigrationIsRefused` | A NULL checksum is backfilled with a 64-character hash; a changed checksum gives `ErrMigrationEdited` |
| `TestMigrationsAreIdempotent` | Migrating an up-to-date schema is a no-op |

Not tested: a failing migration and `SeedDev2025` (exercised by atlas's `TestDevSeedLoads`, teamstate's and server's tests).

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
| `atlas` | `atlas_test` |
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
