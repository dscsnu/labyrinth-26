# labyrinth-26-backend

Go API server backed by plain Postgres. It handles Google sign-in itself and issues its own JWTs. There's no Supabase and no ORM.

## Run locally

```bash
docker compose up -d          # Postgres 17 on localhost:5432
cp .env.example .env          # fill in the Google OAuth client
go run . keygen               # JWT_PRIVATE_KEY, QR_SECRET → .env; JWT_PUBLIC_KEY → ../frontend/.env
go run .                      # applies migrations, then serves on :3000
```

`go run . migrate` applies migrations and exits. Like `go run . seed`, it takes the single-instance lock first, so both **refuse to run while a backend is serving** the same database: stop it first.

### Google OAuth client
Google Cloud Console → APIs & Services → Credentials → **OAuth client ID** (Web application):
- Authorized redirect URI: `PUBLIC_URL` + `/auth/google/callback` (locally `http://localhost:3000/auth/google/callback`)

## Tests

CI (`.github/workflows/ci.yml`) runs everything below, plus gofmt, vet and docaudit, on every push to `main` and every pull request, with **one job per use case** (`test (teamstate)`, `test (server)`, …, built from `go list`, each with its own Postgres, race ×3, no skips) and one aggregate check, `backend`, to require in branch protection (AGENTS.md §14).

```bash
go test ./...                                    # unit tests
docker run -d --rm --name laby-test-pg -e POSTGRES_USER=t -e POSTGRES_PASSWORD=t \
  -e POSTGRES_DB=t -p 55432:5432 postgres:17-alpine
TEST_DATABASE_URL="postgres://t:t@localhost:55432/t?sslmode=disable" go test ./...
```

Every package's tests live in its own `tests/` subfolder (`internal/<package>/tests/`, package `tests`), black-box against the package's exported API; `go test ./internal/teamstate/...` runs one package's. The end-to-end tests in `internal/server/tests` run the full login flow against a fake Google OIDC provider, plus the team and live-stream flows. Store tests in `atlas`, `bosses`, `auth`, `user`, `teams`, `teamstate` and `platform/postgres` each use their own schema. They **wipe schemas** in `TEST_DATABASE_URL`, so never point it at a real database. Two simulations play **100 teams** through the game loop: `TestGameLoopSimulation` (`internal/teamstate/tests`, 90 minutes of game time on a fake clock, 16 strategies, checked against an independent model of the rules) and `TestSim100Teams` (`internal/server/tests`, over real HTTP and event streams with per-phone delays); `go test -run XXX -bench BenchmarkGameLoopCast ./internal/teamstate/tests` measures casts per second with 100 teams at once (AGENTS.md §13).

## Code layout: one package per use case

```
internal/
  atlas/       static game content: waypoints, shapes, patterns (and their spells); read-only, loaded once
  bosses/      static boss sequence (name, asset, description, HP, attack interval and damage) every team fights; read-only, loaded once
  auth/        Google sign-in, access tokens, the auth middleware
  user/        players, helpers and admins
  teams/       teams of up to teams.size (6) players with a leader, lobby → game → finished
  teamstate/   each team's selection, scan stack (wrong scans marked), visited waypoints, completed patterns, cast log, boss progress and health (boss attacks on a timer), powerups and quiz answers; publishes team_state
  qte/         quick-time events (quizzes): content, start and close, GET /api/qte and the game-wide qte event
  leaderboard/ ranks started teams (bosses, then speed, then damage); GET /api/leaderboard and the game-wide leaderboard event
  admin/       every /api/admin/… route, over teams, teamstate, qte and user; owns no data
  spells/      offline geometry and damage formula: projection, TSP tour, convex hull; not imported by server
  server/      wires the use cases together; /health and the live event stream
  platform/    shared plumbing: config, httpx, identity, live (SSE hub), postgres (pool, migrations, lock, seed), stats (runtime/process figures for /health)
cmd/
  spells/      offline damage generator: writes seeds/2026_spells.sql (needs DATABASE_URL, a seeded database)
  loadtest/    load test (see below)
  docaudit/    checks doc comments
```

Every use case has the same four files (except the read-only `atlas` and `bosses`, which have no `write.go`, and the facade `admin`, which owns no data: no `db.go`), plus a `doc.go` and a README describing how each file works, its state, API, and every rule and case. **`AGENTS.md` has the rules for adding or changing code, and new use cases must follow them.** `go run ./cmd/docaudit` checks that every declaration has a doc comment.

| File | Holds |
|---|---|
| `api.go` | its HTTP routes |
| `read.go` | its types and reads |
| `write.go` | its writes |
| `db.go` | its SQL |

`teams` and `teamstate` keep their state **in memory** (`store.go` describes the layout). Reads never touch Postgres, and every write in `write.go` runs the same marked write-through steps: **lock → validate → build → persist (`db.go`) → apply → publish**. Memory only changes after Postgres commits. `auth` and `user` keep nothing in memory; their reads and writes call `db.go` directly. `atlas` holds the static content in memory, built once at startup and never written, so it has no locks and no write-through.

Operational rules that follow from keeping state in memory:
- **Run exactly one backend instance.** A second one refuses to start (Postgres advisory lock), before migrating. The running backend re-checks the lock every **5 s** (`storage.instance_lock_check`) and shuts down (exit 1) if it has lost it, e.g. after a Postgres restart; let your supervisor restart it. Scale up the machine, not out.
- **Don't edit game data with SQL while the backend runs.** Memory won't see the change, and could act on stale data. Use the API, or stop the backend, edit, and start it again. Content (waypoints, shapes, patterns) is only read at startup, by `atlas`.
- `go run . seed` loads the 2026 content (`seeds/2026.sql`: waypoints `2001`–`2010`, 5 shapes, every combination of 2–6 checkpoints as a pattern, 837, each in its **spell class** `class_n` for *n* waypoints), each pattern's damage (`seeds/2026_spells.sql`, generated by `go run ./cmd/spells`) the 6 bosses (`seeds/2026_bosses.sql`) and 4 placeholder quiz drafts, ids 1–4, one per powerup (`seeds/2026_qtes.sql`; a rerun refreshes a draft only while it's unstarted and seeded). It refuses to run while the backend is serving; stop the backend, seed, start it. It replaces any other content (the 2025 placeholders included), and refuses if teams have progress on that content: reset the database (`docker compose down -v`) first. With this content every scan of a checkpoint fits the selected shape, so wrong scans don't happen in play. **An existing database must be reseeded after migrations 0008 and 0012**, or the backend refuses to start (patterns without damage, or without a spell class).

## Load test

```bash
go run ./cmd/loadtest                                  # 10k players, all scenarios (~4 min)
go run ./cmd/loadtest -scenarios ramp -pool 64         # one scenario, Postgres pool of 64
go run ./cmd/loadtest -scenarios play -qr-cooldown 0s  # the game loop, casting flat out
go run ./cmd/loadtest -h                               # all flags
```

It builds the backend and runs it on `:3100` against its own `labyrinth_bench` database, created next to the dev one; your dev data is never touched. Players are inserted directly and their tokens signed with `JWT_PRIVATE_KEY`, so no Google login is involved. The backend under test runs with the per-user rate limit lifted, since a few generator workers stand in for many players; `-rate-limit` keeps `config.toml`'s. Scenarios:

- **rush:** all players form teams of `teams.size` (6) and start, at random moments within `-rush-window` (30 s).
- **sse:** every player holds an event stream during a rush; measures how long a ready-up or start takes to reach teammates.
- **reads:** players poll `GET /api/teams/me` flat out (with every stream still open when `sse` runs too; the two share a backend).
- **ramp:** mixed traffic (90 % reads, 10 % ready toggles, no think time) at rising worker counts until p99 > 500 ms or > 1 % errors.
- **play:** the game loop. A fresh database gets the 2026 content, every player opens an event stream, all teams form and start, then for `-play-duration` (1 min) each team's leader selects a shape (alternating quadrilateral and triangle, as the spell cooldown requires) and its members scan the pattern's checkpoints with QR tokens signed as a `/qr` display signs them (needs `QR_SECRET`), picking only checkpoints the team's QR cooldown leaves free and waiting when too few are. It reports casts, bosses defeated, waits, knockouts and finished teams. `-qr-cooldown 0s` overrides `game.qr_cooldown` for the backend under test, so teams cast as fast as they can.

The load generator and the backend share the machine's CPUs, so results are conservative. On a 16-core laptop (2026-10-21), 10,000 players (1,666 teams, every player streaming):

| Run | Throughput | p99 | Errors | Backend |
|---|---|---|---|---|
| `play`, real 5 min QR cooldown, every team's first casts in the same second | 11,662 scans + 3,332 selects in 1 min | 250 ms | 0 | ≤ 4.3 cores, 675 MB |
| `play -qr-cooldown 0s`, casting flat out for 30 s | 5,318 scans/s + 1,513 selects/s, 45,392 casts, every team finished | 405 ms | 0 (1,646 `not_in_game`: teams that had finished) | ≤ 4.1 cores, 803 MB |

## Deploying

The backend, its Postgres and Caddy (HTTPS and routing) run with Docker Compose on one host, the VM `labyrinth-prod` (project `labyrinth-gdg-2026`, zone `asia-south2-c`). The VM has no checkout of this repository: it runs images from Artifact Registry (`asia-south2-docker.pkg.dev/labyrinth-gdg-2026/labyrinth`), and holds just two files, `compose.prod.yaml` and a `.env` written from Secret Manager.

**1. Build and push the images** (from a machine with this repository, until CI does it):

```bash
gcloud auth configure-docker asia-south2-docker.pkg.dev        # once
TAG=$(git rev-parse --short HEAD)
R=asia-south2-docker.pkg.dev/labyrinth-gdg-2026/labyrinth
docker build --platform linux/amd64 -t $R/backend:$TAG backend   # the VM is x86: build for it from an ARM Mac too
docker build --platform linux/amd64 -t $R/caddy:$TAG caddy
docker push $R/backend:$TAG && docker push $R/caddy:$TAG
gcloud compute scp compose.prod.yaml labyrinth-prod:~/labyrinth/ --zone asia-south2-c   # only when it changed
```

**2. On the VM**, deploy that tag. Once beforehand: Docker with the compose plugin, and `gcloud auth configure-docker asia-south2-docker.pkg.dev` for the user that runs it. The VM's account, `labyrinth-vm`, may read the `labyrinth-*` secrets and pull from the repository, nothing else.

```bash
cd ~/labyrinth
for s in JWT_PRIVATE_KEY QR_SECRET GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET ALLOWED_EMAIL_DOMAINS \
         POSTGRES_PASSWORD FRONTEND_HOST BACKEND_HOST ACME_EMAIL; do
  echo "$s=$(gcloud secrets versions access latest --secret=labyrinth-$s)"
done > .env && echo "IMAGE_TAG=<TAG>" >> .env && chmod 600 .env
dc() { docker compose -f compose.prod.yaml "$@"; }   # shorthand for the lines below
dc pull && dc up -d            # start; also how to deploy a new version (a new IMAGE_TAG)
dc stop backend                # seed (refused while serving), then start again:
dc run --rm backend seed
dc start backend
dc ps                          # backend and postgres should say (healthy); caddy Up
dc logs -f backend             # one line per request; panics at level=ERROR
```

Compose reads `.env` on its own; a missing value stops it, naming the value. A setting changed in Secret Manager reaches the VM when `.env` is rewritten and `dc up -d` runs again. `labyrinth-JWT_PUBLIC_KEY` isn't in the list: it's the frontend's.

**Caddy** (`caddy/Caddyfile`) is the only thing published on the host (80 and 443, plus 443/udp for HTTP/3). It gets and renews HTTPS certificates from Let's Encrypt for two hosts and redirects HTTP to HTTPS:

| Host | Routes to |
|---|---|
| `FRONTEND_HOST` (e.g. `play.<domain>`) | `/api/events` → the backend, flushed as written; everything else → the frontend |
| `BACKEND_HOST` (e.g. `api.<domain>`) | every backend route: sign-in, the API, `/health`, `/docs` |

The hosts and ACME email come from the `labyrinth-FRONTEND_HOST`, `labyrinth-BACKEND_HOST` and `labyrinth-ACME_EMAIL` secrets, and compose builds the backend's `PUBLIC_URL` and `FRONTEND_URL` from the same hosts, so each domain is set once. The Caddyfile is baked into the `caddy` image (`caddy/Dockerfile`): a routing change is a new image. Outside the repo: DNS A records for both hosts pointing at the VM's static IP; the GCP firewall open for TCP 80/443 (the `http-server` and `https-server` network tags) and UDP 443 for HTTP/3; and the Google OAuth client's redirect URI set to `https://<BACKEND_HOST>/auth/google/callback`. Certificates live in the `caddy_data` volume, which must survive deploys: Let's Encrypt issues at most 5 duplicate certificates a week, so `down -v` repeated a few times could leave the site without HTTPS for days.

**Logs** are rotated per container (`x-logging` in the root `compose.prod.yaml`): 100 MB files, at most 50, so at most **5 GB** each, the oldest lines deleted past it. A new container (a deploy, or `down`) starts a fresh log; a crash and restart keeps it. Logging is non-blocking: if writing falls behind, lines are dropped rather than requests slowed.

What it handles on its own (each tested on 2026-10-21):

| Failure | Recovery |
|---|---|
| The backend crashes | `restart: unless-stopped` brings it back with every team's state, read from Postgres (about 1 s). Phones reconnect after 1–6 s and get fresh state. |
| Postgres goes away | The backend loses its instance lock or can't connect, exits, and keeps being restarted; once Postgres is back it recovers by itself. Docker doubles the wait between restarts (up to a minute), so after a 30 s outage it took 23 s more. |
| A deploy (`up -d` with a new `IMAGE_TAG`) | The old container stops before the new one starts, so the single-instance lock never clashes. Never `--scale backend` above 1. |
| The host reboots | All three containers start again with Docker. |

`DATABASE_URL`, `PORT`, `PUBLIC_URL` and `FRONTEND_URL` are set by `compose.prod.yaml`, not read from `.env`. Only Caddy is published on the host. The backend is on the Docker network `labyrinth` as `http://backend:3000`, where Caddy (and later the frontend) reaches it; something in another compose file can join with `networks: { labyrinth: { external: true } }`. Postgres is only on a second, internal network, `db`, shared with the backend alone (`postgres:5432`): nothing on `labyrinth` can reach it, and it has no route out. Run `psql` and `pg_dump` inside the container: `dc exec postgres psql -U labyrinth`. Postgres is tuned for the production VM, a c4-standard-4 (4 vCPU, 15 GB) shared with the frontend: 2 GB shared buffers, 50 connections (the backend's pool takes up to 16, `pool_max_conns` in `DATABASE_URL`, plus one for its instance lock), SSD costs, 256 MB `/dev/shm` and 60 s to stop (the reasoning is in `compose.prod.yaml`); expect it under about 1 GB in use, and check with `docker stats` under load. The backend has a 65,536 open-file limit (one per event stream) and 20 s to shut down. Its healthcheck marks it unhealthy when `/health` fails (Postgres unreachable included) but doesn't restart it: watch `/health` from outside and alert. `.dockerignore` keeps `.env` out of the image build. The image runs as an unprivileged user (uid 10001, `labyrinth`), which can read and run the binary and `config.toml` but not change them. Compose also runs it with a read-only filesystem, every Linux capability dropped and `no-new-privileges`. `config.toml` is baked into the image (`/config.toml`), so a change to it needs a new image and deploy, not just a restart.

Any reverse proxy in front of the backend must do the following; Caddy as configured does each (unbuffered `/api/events`, no proxy read timeout, a 131,072 open-file limit, the route on the frontend's host):

- **Not buffer `/api/events`.** It's a server-sent event stream: each event must reach the phone as it's written. nginx: `proxy_buffering off;` for that location (the backend also sends `X-Accel-Buffering: no` and `Cache-Control: no-store`); Caddy and most load balancers stream `text/event-stream` already. A proxy that buffers holds every event until its buffer fills, and the game looks frozen.
- **Allow idle streams longer than `live.heartbeat` (25 s).** A quiet stream gets a `: ping` comment every 25 s, so a read/idle timeout of **60 s or more** keeps it open (nginx `proxy_read_timeout 1h;`; many cloud load balancers default to 60 s idle, so raise it to several minutes if possible). A stream that's cut reconnects after 1–6 s and gets fresh state, so a lower limit is survivable but wasteful.
- **Hold one long-lived connection per player.** 10,000 players means 10,000 open streams through the proxy, plus the requests: raise its connection limits (nginx `worker_connections` ≥ 16384, and `worker_rlimit_nofile`) and the open-file limit of both the proxy and the backend (`ulimit -n` / systemd `LimitNOFILE=` ≥ 65536; the load-test machine had 524,288). Keep connections to the backend alive (nginx `proxy_http_version 1.1; proxy_set_header Connection "";`) and use HTTP/2 to the phones if possible, so the streams and requests share connections.
- **Route `/api/events` on the frontend's domain to the backend.** The SvelteKit server calls the backend itself (`BACKEND_URL`), but the browser opens the event stream directly at `/api/events` on the frontend's origin, with the `labyrinth_token` cookie (EventSource can't send headers); in dev, Vite's proxy does this (`frontend/vite.config.ts`). Everything else on that domain goes to SvelteKit; the backend's `/auth/google/callback` must be reachable at `PUBLIC_URL`. One origin for the stream means no CORS.
- **Run exactly one backend** (see above): no autoscaling to two instances. Health-check `GET /health`.

The rate limit is per signed-in user, so many players behind one campus IP are fine, and so are requests the SvelteKit server makes on a player's behalf (they all come from its one address, with the player's token). The proxy doesn't need its own per-IP limit for `/api`; one on `/auth` is harmless.

## Migrations

Migrations are plain SQL files in `internal/platform/postgres/migrations/`, named `NNNN_description.sql`. They're embedded in the binary and applied in order at startup, each in its own transaction. Applied versions are recorded in `schema_migrations` with each file's SHA-256.

- Add a new file for every change. **Never edit a file that has already been applied** anywhere shared: the backend refuses to start (`migration was edited after it was applied`) before applying anything.
- An advisory lock stops two instances from migrating at the same time.

## Auth flow (SvelteKit integration)

```
Browser ── GET /auth/google/login?next=/team ──▶ backend ──302──▶ Google
Google  ── GET /auth/google/callback?code&state ──▶ backend
backend ──302──▶ FRONTEND_URL/auth/callback?code=<one-time>&next=/team
SvelteKit server ── POST /auth/token {"code"} ──▶ backend ──▶ {access_token, expires_at, user}
SvelteKit server ── Authorization: Bearer <access_token> ──▶ /api/*
```

- The JWT never shows up in a browser URL. The one-time `code` is single-use, expires after **2 m** (`auth.login_code_ttl`), and is stored only as a SHA-256 hash.
- Google login uses PKCE plus a signed, httpOnly `state` cookie. `next` is restricted to same-site paths (no open redirects).
- Failures redirect to `FRONTEND_URL/auth/error?reason=<reason>` (e.g. `google_access_denied`, `email_domain_not_allowed`, `state_mismatch`, `email_in_use` when another account already has that email).
- JWTs are signed with Ed25519 (`EdDSA`) using `JWT_PRIVATE_KEY`, with lifetime **12 h** (`auth.token_ttl` in config.toml). Claims: `sub` (user id), `email`, `name`, `avatar_url`, `role`.
- The SvelteKit server verifies tokens locally with the public key (`JWT_PUBLIC_KEY`, printed by `go run . pubkey`). It can verify tokens but can't mint them, and page loads make no backend call.
- Tokens are stateless, so logout means the frontend deletes its cookie. Revoking every token means generating a new keypair.
- `role` in the token is a snapshot from sign-in. A promotion or demotion applies when the user signs in again, or within `auth.token_ttl` at the latest. If an endpoint must honour a role change immediately, read the user from the DB, as `/api/me` does.

### What the SvelteKit app needs

1. **Login button:** a plain link (not `fetch`) to `${BACKEND_URL}/auth/google/login?next=/somewhere`.
2. **`src/routes/auth/callback/+server.ts`:**
   ```ts
   export const GET = async ({ url, cookies, fetch }) => {
       const res = await fetch(`${BACKEND_URL}/auth/token`, {
           method: 'POST',
           headers: { 'Content-Type': 'application/json' },
           body: JSON.stringify({ code: url.searchParams.get('code') }),
       });
       if (!res.ok) redirect(303, '/auth/error?reason=token_exchange_failed');
       const { payload } = await res.json();
       cookies.set('labyrinth_token', payload.access_token, {
           path: '/', httpOnly: true, sameSite: 'lax', secure: !dev,
           expires: new Date(payload.expires_at),
       });
       redirect(303, url.searchParams.get('next') ?? '/');
   };
   ```
3. **`hooks.server.ts`:** read `labyrinth_token`, verify it with `jose` (`jwtVerify` with the `EdDSA` public key, issuer `labyrinth-backend`, audience `labyrinth-api`) to fill `locals.user`, and clear the cookie if it fails.
4. **`src/routes/auth/error/+page.svelte`:** show `reason`.
5. **Logout:** delete the `labyrinth_token` cookie.

`/api/*` calls go from the SvelteKit server (load functions and endpoints), so the backend needs no CORS setup.

## Endpoints

Full request and response shapes and every error code: **http://localhost:3000/docs** (Swagger UI), or `internal/server/openapi.yaml`. Tests fail if the spec and the routes or error codes drift apart.


| Method | Path | Auth | Description |
|---|---|---|---|
| GET | `/health` | – | `200` if the database is reachable; payload with `uptime_ms`, `streams`, and runtime/resource stats (`goroutines`, `cpu`, `memory`, …) |
| GET | `/docs` | – | Swagger UI for the whole API (from `/openapi.yaml`) |
| GET | `/openapi.yaml` | – | The OpenAPI 3.1 spec (`internal/server/openapi.yaml`) |
| GET | `/auth/google/login?next=` | – | Starts Google sign-in |
| GET | `/auth/google/callback` | – | Google redirect target |
| POST | `/auth/token` | – | `{"code"}` → `{access_token, token_type, expires_at, user}`; `400 invalid_code`, or `404 user_not_found` if the code's user was deleted |
| GET | `/api/me` | Bearer | Current user (role read fresh from the DB); `401 user_not_found` if the token's user no longer exists |
| GET | `/api/teams/me` | Bearer | Your team, or `404 not_in_team` |
| POST | `/api/teams` | Bearer | `{"name"}` (1 to **32** characters, `teams.name_max_length`) → `201` new team, you as leader |
| POST | `/api/teams/join` | Bearer | `{"team_id"}` (6 digits) → the team |
| POST | `/api/teams/leave` | Bearer | Leave your team (lobby only) |
| PUT | `/api/teams/me/ready` | Bearer | `{"ready": bool}` → the team (non-leaders, lobby only) |
| POST | `/api/teams/me/start` | Bearer | Leader only: `LOBBY` → `IN_GAME` → the team. Every member's stream gets the `IN_GAME` `team`, then the team's empty `team_state` |
| GET | `/api/admin/teams` | Bearer + `ADMIN` | Every team, sorted by ID, each with its game at a glance (`game`: health, knocked out, boss, cooldowns, cast count; `null` in the lobby). Every `/api/admin/…` route is the `admin` use case's; the role is read from Postgres on each request, not from the token (`403 not_admin`) |
| DELETE | `/api/admin/teams/{team_id}/members/{user_id}` | Bearer + `ADMIN` | Remove a member from a team in any status (lobby or started). Removing the leader needs `?new_leader=<user id>`, a remaining member; the only member can't be removed (`409 last_member`). The removed player gets `no_team` and can create or join a team; their scans and casts stay with the team |
| POST | `/api/admin/teams/{team_id}/revive` | Bearer + `ADMIN` | Revive a started team: health back to **10** (`game.health`) and its current boss's attack clock restarted from now; also heals a team that isn't knocked out. Members get `team_state` of type `revived`. `404 team_not_found`, `409 not_in_game` |
| DELETE | `/api/admin/teams/{team_id}` | Bearer + `ADMIN` | Delete a team and all its progress, any status; every member gets `no_team` (`team_deleted`) and is free to join another team |
| POST | `/api/admin/teams/{team_id}/cooldowns/reset` | Bearer + `ADMIN` | Clear a started team's spell class and QR cooldowns (`cooldowns_reset`); later casts start new ones |
| POST | `/api/admin/teams/{team_id}/powerups/{kind}` | Bearer + `ADMIN` | Grant a started team a powerup (`damage_multiplier`, `heart`, `short_qr_cooldown`, `instant_cast`) → `{"granted", "state"}`; `granted` false when it already holds one (at most one of each kind). `404 unknown_powerup`, `team_not_found`; `409 not_in_game` |
| GET | `/api/admin/qtes` | Bearer + `ADMIN` | Every QTE, newest first, in full (the answer too), with its `status` (`draft`, `open`, `closed`) and `stats` (answered, correct, granted, per option) |
| POST | `/api/admin/qtes` | Bearer + `ADMIN` | `{"kind": "quiz", "powerup", "quiz": {"question", "options", "correct_option"}, "window_s"?, "start"}`: a quiz saved as a draft, or sent to every phone at once (`start`), open for `window_s` or **30 s** (`qte.answer_window`; at most **300**, `qte.max_answer_window`). One open at a time (`409 qte_open`); `400 invalid_kind`, `invalid_powerup`, `invalid_question`, `invalid_options`, `invalid_correct_option`, `invalid_window` |
| POST | `/api/admin/qtes/{qte_id}/start` | Bearer + `ADMIN` | Send out a draft (seeded drafts are ids 1–4). `409 qte_already_run`, `qte_open`; `404 unknown_qte` |
| POST | `/api/admin/qtes/{qte_id}/end` | Bearer + `ADMIN` | Close an open QTE now, revealing the answer to everyone. `409 qte_not_started`, `qte_closed` |
| GET | `/api/admin/users[?email=]` | Bearer + `ADMIN` | Up to **20** (`admin.user_search_limit`) users whose email contains `email`, case insensitive |
| PUT | `/api/admin/users/{user_id}/role` | Bearer + `ADMIN` | `{"role": "PLAYER" \| "HELPER" \| "ADMIN"}` → the user. Not your own (`409 cannot_change_own_role`); `400 invalid_role` |
| GET | `/api/leaderboard` | Bearer | The top **10** (`leaderboard.size`) started teams (bosses defeated, then the earliest last defeat, then damage on the current boss) and your own team's entry (`me`). Every stream also gets the top as the game-wide `leaderboard` event, whenever it changes (checked every **5 s**, `leaderboard.publish_every`) |
| GET | `/api/game/shapes` | Bearer | The selectable shapes (`id`, `name`, `patterns` count, distinct `points` sizes), each shape's `patterns_by_points` and `classes` (the spell class of each size), every `spell_classes` entry (`class_1` … by points), `health` (starting health, **10**, `game.health`), `locked_pattern_points` (**5**), `unlock_after_bosses` (**5**) and `max_pattern_points`; the same for every user, static while running |
| GET | `/api/game/bosses` | Bearer | Every boss in serial order (`serial`, `name`, `asset`, `description`, `hp` from the `boss` table; `attack_interval_s`, `attack_damage` from `config.toml` `[game.boss_attack]`: every interval of a team's phase against it, it takes its damage off the team's health): **6** (`game.bosses`), the same for every user, static while running; `[]` before boss content is loaded |
| GET | `/api/teams/me/state` | Bearer | Your team's state (`TeamState`, the same JSON as the `team_state` event's `message`): visited waypoints, completed patterns and shapes, selection, `last_cast` (the newest cast, or `null`), the scan stack (each scan marked `wrong` or not), `poisoned`, boss progress (`bosses_defeated`, `boss {serial, damage, phase_started_at, attacks, next_attack_at}` or `null`, `damage_multiplier`), `health` and `knocked_out`, `qr_cooldowns` (the waypoints the team can't scan yet, each with its `until`), `cast_count` and `version` |
| GET | `/api/teams/me/casts` | Bearer | Your team's cast log: every completed pattern, repeats included (`already_completed`), with its `damage`, newest first |
| PUT | `/api/teams/me/state/selection` | Bearer | `{"shape"}`: select the shape to attempt; always empties the scan stack. **Leader only** (`403 not_leader`). A shape whose patterns are all **5**+ points (`game.locked_pattern_points`) is `409 shape_locked` until the team has defeated **5** bosses (`game.unlock_after_bosses`); `team_state.unlocked_points` and `locked_shapes` say what's open. **Spell cooldown:** the class of the team's last cast can't be cast again until a spell of another class is; a shape whose unlocked patterns are all of it is `409 shape_on_cooldown` (`team_state.cooldown_class`, `cooldown_shapes`) |
| DELETE | `/api/teams/me/state/selection` | Bearer | Give up the selection (and its stack). **Leader only** (`403 not_leader`) |
| POST | `/api/teams/me/state/scans` | Bearer | `{"qr"}` (the waypoint's signed, fresh QR token) → `scan_added`, `wrong_scan` or `pattern_completed`. The token is verified first: `400 invalid_qr` (malformed or tampered), `409 qr_expired` (outside its window). No time limit; any member may scan any number of points. `409 waypoint_already_scanned` if the waypoint is in the stack. **QR cooldown:** for **5 min** (`game.qr_cooldown`) after a cast, its waypoints are `409 waypoint_on_cooldown` for the whole team (nothing recorded; `team_state.qr_cooldowns`); other waypoints stay open. A waypoint that can't be part of any pattern of the shape the team has unlocked is pushed marked wrong (`wrong_scan`), and later scans get `409 selection_poisoned` until a reselect. A knocked-out team (health 0) gets `409 knocked_out` here and on select |
| POST | `/api/teams/me/state/qte/{qte_id}/answer` | Bearer | `{"option": n}`: answer the open quiz for your team. Any member, once per team (`409 qte_already_answered`); right or wrong at once in `team_state.qte`, and a right answer earns the quiz's powerup (unless already held). `409 qte_closed`, `knocked_out`, `not_in_game`; `404 unknown_qte`; `400 invalid_option` |
| GET | `/api/qte` | Bearer | The open quick-time event as players see it (`quiz.correct_option` null), or `null`; the same JSON as the `qte` event |
| POST | `/api/teams/me/state/powerups/{kind}` | Bearer | The leader uses a held powerup: `damage_multiplier` (next cast ×**2**), `heart` (+1 health), `short_qr_cooldown` (casts cool for 2 min, for 5 min), `instant_cast` with `{"pattern": [IDs]}` (cast at once, past the cooldowns) → the team state. `403 not_leader`; `409 powerup_not_held`, `powerup_active`, `full_health`, `knocked_out`, `pattern_locked`; `404 unknown_powerup`, `unknown_pattern` |
| GET | `/api/qr/{waypoint_id}` | Bearer + `HELPER`/`ADMIN` | The waypoint-scoped HMAC key and timing rules (`key`, `rotate_ms`, `window_ms`, `server_time`); the `/qr` display signs its own rotating tokens locally. The role is read from Postgres on each request (`403 not_staff`); `400 invalid_waypoint_id`, `404 unknown_waypoint` |
| POST | `/api/qr/verify` | Bearer | `{"qr"}` → `{"waypoint_id"}`; verifies a scanned token is signed and fresh. Changes nothing. `400 invalid_qr`, `409 qr_expired` |
| GET | `/api/events` | Bearer or `labyrinth_token` cookie | The one server-sent event stream: `hello`, then `team` or `no_team`, then `team_state` if you're in a team, then the game-wide `leaderboard`; then one full snapshot per change (including the changes the server publishes with nobody acting: boss attacks, `boss_attacked` and `knocked_out`, and QR codes opening again, `qr_cooldown_ended`). Every event's data is `{"type", "message"}`: why it was sent (`spell_cast`, `member_joined`, …) and the full state; `team` and `team_state` carry a `version` to keep the newest copy; `: ping` after **25 s** (`live.heartbeat`) with no event; a stream **16** (`live.subscriber_buffer`) events behind, or not taking a write within **10 s** (`live.write_timeout`), is dropped; it reconnects after a random 1–6 s (`live.retry_min`, `live.retry_max`). Full reference: [`internal/server/README.md`, "The event stream"](internal/server/README.md#the-event-stream) |

Responses use the envelope `{"success": bool, "code"?: string, "message"?: string, "payload"?: any}`. `code` is a stable identifier for errors the frontend should handle, e.g. `team_full`; auth's are `invalid_body`, `invalid_code`, `user_not_found` (`/auth/token`) and `missing_token`, `invalid_token` (any Bearer route). `user_not_found` has two statuses: **`404` on `POST /auth/token`** (the login code's user was deleted; no token is involved) and **`401` on `GET /api/me`** (a valid token names a user that no longer exists, so the caller must sign in again). An unknown path is `404 not_found` and a wrong method `405 method_not_allowed`, also as envelopes. A response that fails to encode, or a handler that panics, is a complete `500 internal error`, never a truncated body (a panic is logged with its stack). Every signed-in route is rate limited per user: **10** requests a second in bursts of **40** (`[ratelimit]`); over it, `429 rate_limited` with `Retry-After`. Each request is logged when it finishes (`msg=request route=… status=… ms=…`).

Each use case's README has the full details: rules, error codes and event shapes. The live stream (every event, what triggers it, ordering, heartbeat, reconnection) is documented in [`internal/server/README.md`, "The event stream"](internal/server/README.md#the-event-stream). Everything a team sees about the game arrives as one `team_state` object; see AGENTS.md §17.

## Configuration

Two files:
- **`config.toml`**: every tunable rule and timing (team size, pattern size, the number of bosses `game.bosses`, every team's starting health `game.health`, how bosses attack `[game.boss_attack]` (every `interval`, 10 min, for each boss's `damage` by serial, a nested table), the big-pattern unlock `game.locked_pattern_points` and `game.unlock_after_bosses`, how long a cast's waypoints stay disabled for its team `game.qr_cooldown` (5 min), the powerups' strengths and durations `[powerups]`, the quick-time events' answer windows `[qte]` (30 s, at most 5 min), the rotating-QR window `qr.rotate` and `qr.tolerance`, token lifetimes, heartbeat, server timeouts including `server.read_timeout` (30 s for a whole request), body limit, write timeout, and `storage.instance_lock_check` (5 s between instance-lock checks), the leaderboard's `leaderboard.size` (10) and `leaderboard.publish_every` (5 s), `admin.user_search_limit` (20), and the per-user rate limit `[ratelimit]` `per_second` (10), `burst` (40), `idle` (10 min)). Each key is commented in the file. Read once at startup from `$CONFIG_FILE` (default `./config.toml`; the Docker image sets `/config.toml`); unknown, missing (`missing key <table>.<key>`, even where zero would be valid) or out-of-range values stop the backend from starting. The full key table is in `internal/platform/README.md`. Restart to apply a change; in production the file is baked into the image, so that means a new image and deploy (see "Deploying"). Code never hard-codes these: see AGENTS.md §10.
- **`.env`**: secrets and deployment URLs only. See `.env.example`. Required: `DATABASE_URL`, `JWT_PRIVATE_KEY` (from `go run . keygen`), `QR_SECRET` (from `go run . keygen`), `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `PUBLIC_URL`, `FRONTEND_URL`. Optional: `PORT` (3000), `ALLOWED_EMAIL_DOMAINS` (comma-separated, e.g. `snu.edu.in`).
