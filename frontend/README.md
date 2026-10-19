# labyrinth-26-frontend

## The `/play` page

Where a started team plays. `src/routes/play/+page.server.ts` loads, on the server with the session token:
- `GET /api/game/shapes`: the selectable types (the backend calls them shapes), with each one's pattern count and point sizes, plus `scan_window_ms`, `scan_grace_ms` and `max_pattern_points`.
- `GET /api/teams/me/state`: the team's selection, live stack and progress. A `404` (not in a team) redirects to `/team`; no session redirects to `/`.

`src/routes/play/+page.svelte` then:
1. **Pick a type.** One button per shape from `GET /api/game/shapes` (name, point sizes, pattern count, ✓ if completed). Selecting purges the team's stack; "give up" clears the selection.
2. **Enter waypoints.** For now players type the 4-digit waypoint ID (the dev seed has `1001`–`1011`) instead of scanning a QR code. QR scanning replaces this before the event.
3. **Watch the stack and progress** live: `/team/events` (the proxied backend event stream) delivers `hello` (the clock, see "The countdown"), `team` and `team_state` events. Every `team_state` is the team's whole game state (the same JSON as `GET /api/teams/me/state` and each write's response, `state` in a scan's), so the page just replaces what it shows. When the leader starts, the backend sends the `IN_GAME` `team` and then the team's empty `team_state`, so a page already open has its state without reloading.
   - **No team:** a `no_team` event (the player isn't in a team, e.g. the page was open when they left, or the stream reconnected after their team was gone) closes the stream and sends them to `/team` with `goto`, since `/play` needs a team. This is the live counterpart of the load's `404` redirect.
4. **Last cast.** Under the form, "Last cast: **shape** pattern by member at time" from `last_cast`, with "(cast before)" when `already_completed` is true. It stays until the team's next cast; selecting or clearing doesn't remove it. Nothing is shown while `last_cast` is `null`.

**The stack shrinks on its own.** When a correct scan is more than `scan_window_ms` + `scan_grace_ms` (65 s) old, the backend drops it and publishes a `team_state` without it, with nobody acting. The page doesn't hide scans by time itself: `expires_at` is only for display. When the bottom scan expires, the next one becomes the bottom and sets a new deadline; when the last one does, the empty stack ends the countdown like any other empty stack.

**Wrong scans look normal, on purpose.** A waypoint that doesn't fit any pattern of the selected type is accepted (`200 scan_added`) and shown in the stack like any other, so players can't probe which waypoints are valid. Behind the scenes it poisons the attempt: it never goes stale, and the stack can't complete until the team selects the type again. On the page, that shows up as the countdown running out with no completion. The page offers a way out instead of an explanation:
- **Restart attempt** (shown whenever a type is selected, and when time's up) reselects the same type. The backend purges every scan, wrong ones included, and unlocks every member; any member can press it.
- **Locked after scanning:** every scan is final, so once your scan is in the stack your input is disabled until the attempt casts or someone restarts.
- **"A scan didn't fit":** when the stack is as big as the type's largest pattern and hasn't cast, it must hold a wrong scan (a completed pattern purges the stack), so the page says so and offers a restart. It never says which scan was wrong. See "Rules and cases" in `backend/internal/teamstate/README.md`.

### The countdown
Time is measured one way: the server's clock, estimated from the stream's `hello {server_time}` (`src/lib/game/serverClock.ts`, `ServerClock`):
- on every `hello` (each connect and reconnect), offset = `Date.parse(server_time)` − localNow, where localNow = `performance.timeOrigin + performance.now()` (monotonic)
- `clock.now()` = localNow + offset; before the first `hello` the offset is 0
- the estimate runs behind the server by the delay `hello` took, never ahead; the phone's own clock error cancels out

The rule lives in `src/lib/game/scanGate.ts` (`ScanGate`):
- the deadline is the stack's **bottom** scan's `scanned_at` + `scan_window_ms`, compared against `clock.now()`; `observe()` reads `stack[0].scanned_at` from every `team_state` (and the page load's state)
- every phone gets the same deadline, and reloading the page doesn't change it
- an empty stack (a new selection, a completed pattern, a cleared selection, or the server's expiry event removing the last scan) means no countdown
- when the bottom scan goes stale, the server publishes a `team_state` without it, and the next bottom sets a new deadline
- after the deadline the client sends no more scans; the team selects the type again to restart

The client counts down the window only, never `scan_grace_ms`: the backend's grace covers the `hello` delay plus the request delay of a scan sent just before the end (safe while their sum ≤ grace). The derivation and a worked example are in `backend/internal/teamstate/README.md` ("The client countdown and latency"), and `backend/internal/server/tests/simulation_test.go` mirrors both files in Go: keep them in step.

Tests: `bun run test` (runs `bun test src`, including `src/lib/game/scanGate.test.ts`: 9 tests, `ScanGate` and `ServerClock`).

### The event stream
Both `/team` and `/play` open `new EventSource("/team/events")`. `src/routes/team/events/+server.ts` forwards it to the backend's `GET /api/teams/me/events` with the session token and streams the body back unchanged (`401` without a session, `502` if the backend stream can't be opened). The events (`hello`, `team`, `no_team`, `team_state`), what triggers each, ordering, the `: ping` heartbeat and reconnection are documented in [`backend/internal/server/README.md`, "The event stream"](../backend/internal/server/README.md#the-event-stream). `EventSource` reconnects by itself, and the first events after each connect are full snapshots, so the pages need no catch-up logic. The data types are in `src/lib/types/Game.type.ts` (`TeamStateSchema`, including `last_cast`) and `src/lib/types/Team.type.ts`.

### Proxy routes
The browser never calls the backend directly. These SvelteKit endpoints forward its requests with the httpOnly session token (`proxyJSON` in `src/lib/server/backend.ts`), returning the backend's status and envelope unchanged; without a session they answer `401 not_signed_in`:

| Browser calls | Forwards to |
|---|---|
| `POST /play/scan` `{"waypoint_id"}` (`src/routes/play/scan/+server.ts`) | `POST /api/teams/me/state/scans` |
| `PUT /play/selection` `{"shape"}` (`src/routes/play/selection/+server.ts`) | `PUT /api/teams/me/state/selection` |
| `DELETE /play/selection` (same file) | `DELETE /api/teams/me/state/selection` |
