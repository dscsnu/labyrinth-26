# labyrinth-26-frontend

## The `/play` page

Where a started team plays. `src/routes/play/+page.server.ts` loads, on the server with the session token:
- `GET /api/game/shapes`: the selectable types (the backend calls them shapes), with each one's pattern count, point sizes and damage range, plus `max_pattern_points`. There's no time limit, so no timing rules.
- `GET /api/teams/me/state`: the team's selection, stack and progress. A `404` (not in a team) redirects to `/team`; no session redirects to `/`.
- `GET /api/teams/me/casts`: the team's cast log, newest first.

`src/routes/play/+page.svelte` then:
1. **Pick a type.** One button per shape from `GET /api/game/shapes` (name, point sizes, damage range, pattern count, ✓ if completed). Selecting purges the team's stack; "give up" clears the selection.
2. **Enter waypoints.** For now players type the 4-digit waypoint ID (the dev seed has `1001`–`1011`) instead of scanning a QR code. QR scanning replaces this before the event. There's no time limit, and any member may enter any number of the pattern's waypoints; the same waypoint twice is `409 waypoint_already_scanned`.
3. **Watch the stack and progress** live: `/api/events` (the backend's event stream) delivers `hello` (the clock, see "Time"), `team` and `team_state` events. Every `team_state` is the team's whole game state (the same JSON as `GET /api/teams/me/state` and each write's response, `state` in a scan's), so the page just replaces what it shows. When the leader starts, the backend sends the `IN_GAME` `team` and then the team's empty `team_state`, so a page already open has its state without reloading. Nothing changes with time alone: every `team_state` follows someone's action.
   - **No team:** a `no_team` event (the player isn't in a team, e.g. the page was open when they left, or the stream reconnected after their team was gone) closes the stream and sends them to `/team` with `goto`, since `/play` needs a team. This is the live counterpart of the load's `404` redirect.
4. **Casts.** The cast log from the load, newest first: "**shape** pattern (damage) by member, time", with "(cast before)" when `already_completed` is true. The log isn't pushed on the stream; instead, whenever a `team_state` (or a scan's response) carries a `last_cast` the list doesn't have yet (matched by `cast_at`), the page prepends it. A reload refetches the whole log.

**Wrong scans are reported.** A waypoint that, with the scans already in the stack, can't be part of any pattern of the selected type is still accepted (`200`), but with outcome `wrong_scan`:
- the scanner sees, in red, that the waypoint doesn't match any pattern of that type with the points already scanned
- the scan shows in the stack in red, marked "✗ doesn't fit" (`stack[].wrong`), on every phone
- `team_state.poisoned` is `true`: the page shows a red notice with a **restart attempt** button and disables the input; the backend refuses further scans with `409 selection_poisoned` anyway
- **Restart attempt** (also shown whenever a type is selected) reselects the same type: the backend purges every scan, the wrong one included. Any member can press it.

See "Rules and cases" in `backend/internal/teamstate/README.md`.

### Time
There's no time limit. Time is only displayed: each stack scan shows how long ago it was scanned ("12 s ago", "3 min ago"), refreshed every second. It's measured one way, the server's clock, estimated from the stream's `hello {server_time}` (`src/lib/game/serverClock.ts`, `ServerClock`), so every phone shows the same:
- on every `hello` (each connect and reconnect), offset = `Date.parse(server_time)` − localNow, where localNow = `performance.timeOrigin + performance.now()` (monotonic)
- `clock.now()` = localNow + offset; before the first `hello` the offset is 0
- the estimate runs behind the server by the delay `hello` took, never ahead; the phone's own clock error cancels out

Tests: `bun run test` (runs `bun test src`, including `src/lib/game/serverClock.test.ts`: 4 tests of `ServerClock`).

### The event stream
Both `/team` and `/play` open `new EventSource("/api/events")` on this app's own origin; the browser sends the httpOnly `labyrinth_token` cookie, which the backend accepts on that one route. In production the shared domain's reverse proxy routes `/api/events` to the backend; in dev, Vite's dev server does (`vite.config.ts`, `server.proxy`, to `BACKEND_URL`), so restart `bun run dev` after changing `BACKEND_URL`. There's no SvelteKit route for the stream: SvelteKit never copies it. The events (`hello`, `team`, `no_team`, `team_state`), what triggers each, ordering, the `: ping` heartbeat and reconnection are documented in [`backend/internal/server/README.md`, "The event stream"](../backend/internal/server/README.md#the-event-stream). `EventSource` reconnects by itself, and the first events after each connect are full snapshots, so the pages need no catch-up logic. The data types are in `src/lib/types/Game.type.ts` (`TeamStateSchema`, including `last_cast` and `poisoned`; `CastLogSchema`) and `src/lib/types/Team.type.ts`.

### Proxy routes
Apart from the event stream (`/api/events`, above), the browser never calls the backend directly. These SvelteKit endpoints forward its requests with the httpOnly session token (`proxyJSON` in `src/lib/server/backend.ts`), returning the backend's status and envelope unchanged; without a session they answer `401 not_signed_in`:

| Browser calls | Forwards to |
|---|---|
| `POST /play/scan` `{"waypoint_id"}` (`src/routes/play/scan/+server.ts`) | `POST /api/teams/me/state/scans` |
| `PUT /play/selection` `{"shape"}` (`src/routes/play/selection/+server.ts`) | `PUT /api/teams/me/state/selection` |
| `DELETE /play/selection` (same file) | `DELETE /api/teams/me/state/selection` |
| `GET /qr/{waypoint_id}` (`src/routes/qr/[waypoint]/+server.ts`) | `GET /api/qr/{waypoint_id}` |
| `POST /qr/verify` `{"qr"}` (`src/routes/qr/verify/+server.ts`) | `POST /api/qr/verify` |

## The `/admin` page
For organisers: removing a member from a team. The home page links to it when the session's role is `ADMIN`, but the link is only a convenience. **The backend decides who's an admin**, from the role in Postgres on every request (`403 not_admin` otherwise), because the session's role is as of sign-in and may be stale.

`src/routes/admin/+page.server.ts`:
- **load:** no session redirects to `/`. `GET /api/admin/teams` with the session token; a `403` becomes SvelteKit's `error(403, 'Admins only')`. Returns every team (`TeamSchema[]`).
- **`remove` action** (form fields `team_id`, `user_id`, `new_leader`): `DELETE /api/admin/teams/{team_id}/members/{user_id}`, adding `?new_leader=` when given. Success returns `{removed}`; a backend error comes back as `fail(status, {code, message})`, shown in red.

`src/routes/admin/+page.svelte`:
- Every team with its code, status and members, filterable by code, team name or player name.
- Each member has a **remove** button. The leader's has a required **new leader** picker listing the other members, since the backend refuses a leader's removal without one (`new_leader_required`). A team's only member shows "can't be removed" (the backend's `last_member`).
- The form uses `use:enhance`; its submit function asks `confirm()` first and calls `cancel()` on "no". After a removal SvelteKit reloads the page's data, so the list is current. The page doesn't listen to events: it shows the teams as of the last load or action.

What the removal does to players: the removed player's stream gets `no_team` (so `/team` shows "No team" and `/play` sends them to `/team`); the rest get the smaller `team`, and a started team keeps playing. See [`backend/internal/teams/README.md`, "Remove a member (admin)"](../backend/internal/teams/README.md#remove-a-member-admin-delete-apiadminteamsteam_idmembersuser_id).

## The `/qr` display page

For staff: a screen at a waypoint showing its rotating QR. `src/routes/qr/+page.svelte`:

1. Enter a 4-digit waypoint ID. The page fetches `GET /qr/{waypoint_id}` **once**, which the backend answers with the waypoint-scoped HMAC signing key and timing rules (`key`, `rotate_ms`, `window_ms`, `server_time`) — see `backend/internal/qr/README.md`.
2. It signs `"<waypoint>:<unix seconds>"` locally with Web Crypto (`crypto.subtle`, HMAC-SHA256), base64url-encodes the signature, and renders the token as a QR (`qrcode`), re-signing every `rotate_ms`. `server_time` corrects the display's own clock before signing.
3. No further requests: the QR rotates from the one key, offline except for the initial fetch.

`src/routes/qr/+page.server.ts` gates the page to staff: it checks the backend role via `GET /api/me` (HELPER/ADMIN), matching the backend route `GET /api/qr/{waypoint_id}` (`403 not_staff`). The page is handed a signing key, so it must not be reachable by players.

## The `/test-scan` page

A dev/test harness for the QR flow. `src/routes/test-scan/+page.svelte` opens the camera (`getUserMedia`), decodes a QR every ~250 ms (`jsqr`), and posts the token to `POST /qr/verify`; the backend verifies the signature and freshness and returns the waypoint, or `invalid_qr` / `qr_expired`. It changes nothing (no team state), so it's usable by any signed-in user.
