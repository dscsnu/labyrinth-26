# labyrinth-26-frontend

## The `/play` page

Where a started team plays. `src/routes/play/+page.server.ts` loads, on the server with the session token:
- `GET /api/game/shapes`: the selectable types (the backend calls them shapes), with each one's pattern count, point sizes and damage range, plus `health` (every team's starting health, **10**, `game.health`) and `max_pattern_points`. There's no time limit on an attempt.
- `GET /api/teams/me/state`: the team's selection, stack and progress. A `404` (not in a team) redirects to `/team`; no session redirects to `/`.
- `GET /api/teams/me/casts`: the team's cast log, newest first.
- `GET /api/game/bosses`: every boss in serial order (name, asset, description, HP, and `attack_interval_s`, `attack_damage` from the backend's `config.toml` `[game.boss_attack]`); static, so loaded once.

`src/routes/play/+page.svelte` then:
0. **Health.** "Health: " and a `<progress>` bar of `team_state.health` out of `game.health`, "n/10". When `knocked_out` is true, a red banner: "**Knocked out.** Your team can't select or scan until an organiser revives it", and the type buttons and the scan form are disabled (the backend refuses them with `409 knocked_out` anyway).
   **The boss.** "Boss n/7" (`bosses_defeated` + 1 of `bosses.length`), then the current boss, looked up in the loaded list by `team_state.boss.serial`: its image, name and description, and an HP bar (`<progress>`) of `hp − boss.damage` out of `hp`, with "damage ×m" when `damage_multiplier` isn't 1. Under it, while `boss.next_attack_at` isn't `null`: "Attacks every m min for d: next in **m:ss**", a countdown to `next_attack_at` on the server's clock (see "Time"), refreshed every second. The countdown only displays: the attack itself arrives as a `boss_attacked` event when the backend applies it. When `boss` is `null` it shows "Every boss defeated!" (or "No bosses loaded." if the list is empty). The image is `/<asset>`, e.g. `/bosses/1.png`, served from `static/bosses/<serial>.png`; **none exist yet**, and a missing image hides itself (`onerror`).
1. **Pick a type.** One button per shape from `GET /api/game/shapes` (name, point sizes, damage range, pattern count, ✓ if completed). **Only the leader** can use them (`team.leader_id` = the user; the backend answers anyone else `403 not_leader`): other members see "Your leader picks the type; you scan." and disabled buttons, and **restart attempt** and **give up** are the leader's too. A shape in `team_state.locked_shapes` shows a 🔒 and is disabled (the backend would answer `409 shape_locked`); while `unlocked_points` is below `game.max_pattern_points`, a note says "Patterns of 5+ points unlock after defeating 5 bosses (n/5)" (`game.locked_pattern_points`, `game.unlock_after_bosses`, `bosses_defeated`). The buttons update live: the unlock arrives in the `boss_defeated` event of the fifth defeat. Selecting purges the team's stack; "give up" clears the selection.
2. **Enter waypoints.** For now players type the 4-digit waypoint ID (the dev seed has `1001`–`1011`) instead of scanning a QR code. QR scanning replaces this before the event. There's no time limit, and any member may enter any number of the pattern's waypoints; the same waypoint twice is `409 waypoint_already_scanned`.
3. **Watch the stack and progress** live: `/api/events` (the backend's event stream) delivers `hello` (the clock, see "Time"), `team` and `team_state` events. Every `team_state` is the team's whole game state (the same JSON as `GET /api/teams/me/state` and each write's response, `state` in a scan's), so the page just replaces what it shows. When the leader starts, the backend sends the `IN_GAME` `team` and then the team's empty `team_state`, so a page already open has its state without reloading. The one `team_state` that follows nobody's action is a boss attack (`boss_attacked`, `knocked_out`), which the backend publishes on its own timer.
   - **No team:** a `no_team` event (the player isn't in a team, e.g. the page was open when they left, or the stream reconnected after their team was gone) closes the stream and sends them to `/team` with `goto`, since `/play` needs a team. This is the live counterpart of the load's `404` redirect.
4. **Casts.** The cast log from the load, newest first: "**shape** pattern (damage, dealt to boss, "defeated it") by member, time", with "(cast before)" when `already_completed` is true. The log isn't pushed on the stream; instead, whenever a `team_state` (or a scan's response) carries a `last_cast` the list doesn't have yet (matched by `cast_at`), the page prepends it. When `cast_count` is more than the list's length (casts missed while disconnected), it fetches the whole log again through `GET /play/casts`.
5. **Boss moments.** A scan that casts says "completed … : 107 dmg to Matthew Patel" (", defeated!" if it finished the boss). A `team_state` event of type `boss_defeated` (a teammate's cast that finished a boss) shows "<boss> defeated!" on every phone. `boss_attacked` shows "<boss> attacked! Health n/10" in red, `knocked_out` "Knocked out! Ask an organiser to revive your team.", and `revived` "Revived: full health". Moments are shown only for a copy that's newer than the one held (a repeated or stale event shows nothing).

**Every copy goes through one guard.** Stream events and write responses both call `observe`, which keeps a `team_state` only if `isNewer(held.version, incoming.version)` (`src/lib/types/Live.type.ts`: a different `epoch`, or a higher `seq`), since a response and its event can arrive in either order. The `team` event is kept the same way, or always when it's a different team.

**Wrong scans are reported.** A waypoint that, with the scans already in the stack, can't be part of any pattern of the selected type is still accepted (`200`), but with outcome `wrong_scan`:
- the scanner sees, in red, that the waypoint doesn't match any pattern of that type with the points already scanned
- the scan shows in the stack in red, marked "✗ doesn't fit" (`stack[].wrong`), on every phone
- `team_state.poisoned` is `true`: the page shows a red notice with a **restart attempt** button (enabled for the leader only) and disables the input; the backend refuses further scans with `409 selection_poisoned` anyway
- **Restart attempt** (also shown whenever a type is selected) reselects the same type: the backend purges every scan, the wrong one included. Any member can press it.

See "Rules and cases" in `backend/internal/teamstate/README.md`.

### Time
There's no time limit on an attempt. Time is only displayed: each stack scan shows how long ago it was scanned ("12 s ago", "3 min ago"), and the boss panel counts down to `boss.next_attack_at`, both refreshed every second. The backend applies attacks; the page never changes state because a countdown reached zero. It's measured one way, the server's clock, estimated from the stream's `hello {server_time}` (`src/lib/game/serverClock.ts`, `ServerClock`), so every phone shows the same:
- on every `hello` (each connect and reconnect), offset = `Date.parse(server_time)` − localNow, where localNow = `performance.timeOrigin + performance.now()` (monotonic)
- `clock.now()` = localNow + offset; before the first `hello` the offset is 0
- the estimate runs behind the server by the delay `hello` took, never ahead; the phone's own clock error cancels out

Tests: `bun run test` (runs `bun test src`, including `src/lib/game/serverClock.test.ts`: 4 tests of `ServerClock`).

### The event stream
Both `/team` and `/play` open `new EventSource("/api/events")` on this app's own origin (one each; a single shared connection for the app isn't done yet); the browser sends the httpOnly `labyrinth_token` cookie, which the backend accepts on that one route. In production the shared domain's reverse proxy routes `/api/events` to the backend; in dev, Vite's dev server does (`vite.config.ts`, `server.proxy`, to `BACKEND_URL`), so restart `bun run dev` after changing `BACKEND_URL`. There's no SvelteKit route for the stream: SvelteKit never copies it. The events (`hello`, `team`, `no_team`, `team_state`), what triggers each, ordering, the `: ping` heartbeat and reconnection are documented in [`backend/internal/server/README.md`, "The event stream"](../backend/internal/server/README.md#the-event-stream). `EventSource` reconnects by itself, and the first events after each connect are full snapshots, so the pages need no catch-up logic.

**Every event's data is an envelope** `{"type", "message"}`: `type` is why it was sent (`snapshot`, `spell_cast`, `boss_defeated`, `member_joined`, …), `message` the whole resource. The pages parse it with `envelope(schema)` from `src/lib/types/Live.type.ts` and render from `message`, using `type` only for moments (the `boss_defeated`, `boss_attacked`, `knocked_out` and `revived` notices). `hello`'s clock is `message.server_time`. `team` and `team_state` carry `version` (`VersionSchema`), compared with `isNewer`.

The data types are in `src/lib/types/Game.type.ts` (`GameSchema` with `health`, `locked_pattern_points` and `unlock_after_bosses`, each shape with `patterns_by_points`; `TeamStateSchema`, including `last_cast`, `poisoned`, `bosses_defeated`, `boss` (with `phase_started_at`, `attacks`, `next_attack_at`), `damage_multiplier`, `health`, `knocked_out`, `unlocked_points`, `locked_shapes`, `cast_count` and `version`; `CastSchema` with `dealt`, `boss`, `defeated_boss`; `CastLogSchema`; `BossSchema` with `attack_interval_s` and `attack_damage`, `BossListSchema`), `src/lib/types/Team.type.ts` (`TeamSchema`, with `version`) and `src/lib/types/Live.type.ts`.

### Proxy routes
Apart from the event stream (`/api/events`, above), the browser never calls the backend directly. These SvelteKit endpoints forward its requests with the httpOnly session token (`proxyJSON` in `src/lib/server/backend.ts`), returning the backend's status and envelope unchanged; without a session they answer `401 not_signed_in`:

| Browser calls | Forwards to |
|---|---|
| `POST /play/scan` `{"waypoint_id"}` (`src/routes/play/scan/+server.ts`) | `POST /api/teams/me/state/scans` |
| `PUT /play/selection` `{"shape"}` (`src/routes/play/selection/+server.ts`) | `PUT /api/teams/me/state/selection` |
| `DELETE /play/selection` (same file) | `DELETE /api/teams/me/state/selection` |
| `GET /play/casts` (`src/routes/play/casts/+server.ts`) | `GET /api/teams/me/casts` (the cast log, re-fetched when `cast_count` says casts were missed) |
| `GET /qr/{waypoint_id}` (`src/routes/qr/[waypoint]/+server.ts`) | `GET /api/qr/{waypoint_id}` |
| `POST /qr/verify` `{"qr"}` (`src/routes/qr/verify/+server.ts`) | `POST /api/qr/verify` |

## The `/admin` page
For organisers: every team at a glance, and the admin tools. The home page links to it when the session's role is `ADMIN`, but the link is only a convenience. **The backend decides who's an admin**, from the role in Postgres on every request (`403 not_admin` otherwise), because the session's role is as of sign-in and may be stale. Every route it calls is the backend's `admin` use case ([`backend/internal/admin/README.md`](../backend/internal/admin/README.md)).

`src/routes/admin/+page.server.ts`:
- **load:** no session redirects to `/`. `GET /api/admin/teams` (each team with its `game` at a glance: `TeamOverviewListSchema`); a `403` becomes SvelteKit's `error(403, 'Admins only')`. With `?email=` in the page's URL it also searches users (`GET /api/admin/users?email=`, `UserListSchema`) and returns them as `users` (`null` without a search).
- **Actions**, all through `adminCall` (a backend error comes back as `fail(status, {code, message})`, shown in red; success as `{done: "…"}`, shown in green):
  - `revive` (`team_id`): `POST /api/admin/teams/{team_id}/revive`
  - `reset` (`team_id`): `POST /api/admin/teams/{team_id}/cooldowns/reset`
  - `delete` (`team_id`): `DELETE /api/admin/teams/{team_id}`
  - `remove` (`team_id`, `user_id`, `new_leader`): `DELETE /api/admin/teams/{team_id}/members/{user_id}`, adding `?new_leader=` when given
  - `role` (`user_id`, `role`): `PUT /api/admin/users/{user_id}/role {"role"}`

`src/routes/admin/+page.svelte`:
- **Teams:** every team with its code, status and members, filterable by code, team name or player name, and by a **knocked out only** checkbox (with the count). A started team shows its game: **KNOCKED OUT** in red, health, bosses defeated, the current boss and its damage, casts, the class on cooldown and the waypoints cooling.
- **Per team:** **revive (full health)** and **reset cooldowns** (disabled when nothing is cooling) on an `IN_GAME` team; **delete team** on any. Each member has a **remove** button; the leader's has a required **new leader** picker (the backend refuses a leader's removal without one: `new_leader_required`); a team's only member shows "can't be removed" (`last_member`).
- **Users and roles:** a search form (`GET` with `?email=`) and, per user found, a role picker and **set role**; your own row says another admin must change your role (the backend's `cannot_change_own_role`).
- Every write form uses `use:enhance` with `confirmFirst(question)`, which asks `confirm()` and calls `cancel()` on "no" (the role form doesn't ask). After an action SvelteKit reloads the page's data, so the list is current. The page doesn't listen to events: it shows the teams as of the last load or action.

What the actions do to players: a removed player's stream gets `no_team` (`/team` shows "No team", `/play` sends them to `/team`), the rest get the smaller `team`; a deleted team's members all get `no_team`; a revive or reset reaches the team as `team_state` (`revived`, `cooldowns_reset`). See [`backend/internal/teams/README.md`](../backend/internal/teams/README.md) and [`backend/internal/teamstate/README.md`](../backend/internal/teamstate/README.md).

The types are in `src/lib/types/Admin.type.ts` (`GameOverviewSchema`, `TeamOverviewSchema`, `TeamOverviewListSchema`, `UserListSchema`).

## The `/leaderboard` page
The top teams, live. The home page links to it.

- `src/routes/leaderboard/+page.server.ts`: no session redirects to `/`; `GET /api/leaderboard` (`LeaderboardMineSchema`: the top, the number of ranked teams, and your own team's entry `me`).
- `src/routes/leaderboard/+page.svelte`: a table of the top (rank, team, bosses, when the last boss fell, damage on the current boss; 🏆 for a finished team; your team in bold). It opens `/api/events` and replaces the top with each game-wide `leaderboard` event's `message` (`snapshot` on connect, `updated` when the top changed, at most every **5 s**, `leaderboard.publish_every`). Your own entry is the page load's, shown below the table when you're outside the top; reload to refresh it.
- The types are in `src/lib/types/Leaderboard.type.ts` (`LeaderboardEntrySchema`, `LeaderboardBoardSchema`, `LeaderboardMineSchema`).

## The `/qr` display page

For staff: a screen at a waypoint showing its rotating QR. `src/routes/qr/+page.svelte`:

1. Enter a 4-digit waypoint ID. The page fetches `GET /qr/{waypoint_id}` **once**, which the backend answers with the waypoint-scoped HMAC signing key and timing rules (`key`, `rotate_ms`, `window_ms`, `server_time`) — see `backend/internal/qr/README.md`.
2. It signs `"<waypoint>:<unix seconds>"` locally with Web Crypto (`crypto.subtle`, HMAC-SHA256), base64url-encodes the signature, and renders the token as a QR (`qrcode`), re-signing every `rotate_ms`. `server_time` corrects the display's own clock before signing.
3. No further requests: the QR rotates from the one key, offline except for the initial fetch.

`src/routes/qr/+page.server.ts` gates the page to staff: it checks the backend role via `GET /api/me` (HELPER/ADMIN), matching the backend route `GET /api/qr/{waypoint_id}` (`403 not_staff`). The page is handed a signing key, so it must not be reachable by players.

## The `/test-scan` page

A dev/test harness for the QR flow. `src/routes/test-scan/+page.svelte` opens the camera (`getUserMedia`), decodes a QR every ~250 ms (`jsqr`), and posts the token to `POST /qr/verify`; the backend verifies the signature and freshness and returns the waypoint, or `invalid_qr` / `qr_expired`. It changes nothing (no team state), so it's usable by any signed-in user.
