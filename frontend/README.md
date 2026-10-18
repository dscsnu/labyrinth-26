# labyrinth-26-frontend

## The `/play` page

Where a started team plays. `src/routes/play/+page.server.ts` loads, on the server with the session token:
- `GET /api/game/shapes`: the selectable types (the backend calls them shapes), with each one's pattern count and point sizes, plus `scan_window_ms`, `scan_grace_ms` and `max_pattern_points`.
- `GET /api/teams/me/state`: the team's selection, live stack and progress. A `404` (not in a team) redirects to `/team`; no session redirects to `/`.

`src/routes/play/+page.svelte` then:
1. **Pick a type.** One button per shape from `GET /api/game/shapes` (name, point sizes, pattern count, ✓ if completed). Selecting purges the team's stack; "give up" clears the selection.
2. **Enter waypoints.** For now players type the 4-digit waypoint ID (the dev seed has `1001`–`1011`) instead of scanning a QR code. QR scanning replaces this before the event.
3. **Watch the stack and progress** live: `/team/events` (the proxied backend event stream) delivers `team` and `team_state` events.

**Wrong scans look normal, on purpose.** A waypoint that doesn't fit any pattern of the selected type is accepted (`200 scan_added`) and shown in the stack like any other, so players can't probe which waypoints are valid. Behind the scenes it poisons the attempt: it never goes stale, and the stack can't complete until the team selects the type again. On the page, that shows up as the countdown running out with no completion. See "Rules and cases" in `backend/internal/teamstate/README.md`.

### The countdown
The rule lives in `src/lib/game/scanGate.ts` (`ScanGate`):
- it starts when this client first sees a non-empty stack: its own `200` for the first scan, or a teammate's scan arriving as a `team_state` event
- it resets whenever the client sees an empty stack (a new selection, a completed pattern, a cleared selection)
- after `scan_window_ms` the client sends no more scans; the team selects the type again to restart

The client counts down the window only, never `scan_grace_ms`: the backend's grace covers the round trip of a scan sent just before the end (safe while response delay + request delay ≤ grace). The derivation and a worked example are in `backend/internal/teamstate/README.md` ("The client countdown and latency"), and `backend/internal/server/tests/simulation_test.go` mirrors this rule in Go: keep the two in step.

When the page loads mid-attempt, the countdown is started from the bottom scan's `scanned_at`, so it relies on the phone's clock roughly matching the server's.

Tests: `bun run test` (runs `bun test src`, including `src/lib/game/scanGate.test.ts`).

### Proxy routes
The browser never calls the backend directly. These SvelteKit endpoints forward its requests with the httpOnly session token (`proxyJSON` in `src/lib/server/backend.ts`), returning the backend's status and envelope unchanged; without a session they answer `401 not_signed_in`:

| Browser calls | Forwards to |
|---|---|
| `POST /play/scan` `{"waypoint_id"}` (`src/routes/play/scan/+server.ts`) | `POST /api/teams/me/state/scans` |
| `PUT /play/selection` `{"shape"}` (`src/routes/play/selection/+server.ts`) | `PUT /api/teams/me/state/selection` |
| `DELETE /play/selection` (same file) | `DELETE /api/teams/me/state/selection` |
