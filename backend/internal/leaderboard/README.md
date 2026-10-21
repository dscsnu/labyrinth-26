# Leaderboard: who's winning

Ranks the started teams: bosses defeated, then who got there first, then damage on the current boss. It **stores nothing**: the ranking is derived from team state's standings and the teams' names whenever it's needed, from memory. `GET /api/leaderboard` returns the top and the caller's own team; a background loop broadcasts the top as the game-wide `leaderboard` event whenever it changes.

Config values appear with their key: **10** (`leaderboard.size`).

## Contents
1. [Concepts](#concepts)
2. [The flow](#the-flow)
3. [How each file works](#how-each-file-works)
4. [Data model](#data-model)
5. [The write-through](#the-write-through)
6. [API](#api)
7. [Rules and cases](#rules-and-cases)
8. [Concurrency and locking](#concurrency-and-locking)
9. [Failure and recovery](#failure-and-recovery)
10. [Limits and how to change them](#limits-and-how-to-change-them)
11. [Not decided yet](#not-decided-yet)
12. [Tests](#tests)

## Concepts
| Term | Meaning |
|---|---|
| **Ranked team** | A team that has started: `IN_GAME` or `FINISHED`. Lobby teams aren't ranked. |
| **Standing** | A team's progress as teamstate reports it (`teamstate.Standing`): bosses defeated, when its latest boss fell, damage on its current boss. |
| **Rank** | 1, 2, 3, …; every team has its own (ties break by team ID), so the order is the same for everyone. |
| **Top** | The first **10** (`leaderboard.size`) ranked teams: what the event carries. |
| **Finished** | The team defeated every boss (`FINISHED`, set by teams' `Finish`); marked 🏆 on `/leaderboard`. |

## The flow
1. Teams cast; teamstate's state changes.
2. Every **5 s** (`leaderboard.publish_every`) `Run` ranks every team; if the top differs from the last broadcast, it broadcasts it (`leaderboard` event, type `updated`) to every stream.
3. A stream opens with the current top (type `snapshot`), after the user's own events.
4. `GET /api/leaderboard` ranks now and adds the caller's own entry, even outside the top.

**Why ranked on demand, not stored.** Everything the ranking reads is already in memory (teamstate's snapshots, teams' names). Ranking ~1,700 teams is a sort of ~1,700 entries: well under a millisecond, so there's nothing worth caching or keeping in step.

**Why the event carries only the top, and only on a change.** A full ranking is ~100 bytes a team; sent to every phone (~10,000) on every cast it would be gigabytes. The top is ~1 KB, sent at most once per interval and only when it changed.

**Why bosses, then time, then damage.** Bosses defeated is the game's progress. Between teams on the same boss count, the one that got there first is ahead (so a team can't overtake by waiting). Damage on the current boss separates teams still racing to the next one.

## How each file works
### `doc.go`
The package comment.

### `read.go`
- **`Standings`** interface: `Standings() []teamstate.Standing` (`*teamstate.Store`).
- **`Teams`** interface: `All()`, `ForUser(userID)` (`*teams.Store`).
- **`Entry`**, **`Board`** (`top`, `teams`), **`Mine`** (`Board` plus `me`).
- **`rank()`**: every non-lobby team from `Teams.All()`, joined with its standing (zero if it has none: it hasn't done anything), sorted by:
  1. bosses defeated, more first
  2. latest defeat's time, earlier first; a team with none after any with one
  3. damage on the current boss, more first
  4. team ID

  then numbered from 1.
- **`board(ranked)`**: the first `Size` entries and the count.
- **`ForUser(userID)`**: ranks now; `Me` is the caller's team's entry, or nil if they have no ranked team.

### `store.go`
- **`Rules`**: `Size` (`leaderboard.size`), `PublishEvery` (`leaderboard.publish_every`).
- **`Leaderboard`**, **`New(standings, teams, hub, log, rules)`**.
- **`Refresh()`**: ranks, encodes the top; if it differs from `last`, builds the event (`live.NewEvent("leaderboard", Updated, board)`), stores `last` and broadcasts, all under `mu` (so broadcasts go out in order). Reports whether it broadcast.
- **`Run(ctx)`**: a ticker every `PublishEvery` calling `Refresh`, until `ctx` ends. `main` starts it (`go app.Leaderboard.Run(ctx)`).
- **`Snapshot()`**: the current top as a `snapshot` event, for `server.snapshot`.

### `api.go`
- **`Routes(mux, authed)`**: `GET /api/leaderboard` → `ForUser(identity.UserID(r))`.

## Data model
**Memory:** `last []byte`, the last broadcast `Board` as JSON (guarded by `mu`), only to tell whether the top changed. Memory-only: after a restart the first `Refresh` broadcasts (every stream got a fresh snapshot anyway). **Postgres:** none.

`Entry` (JSON):

| Field | Type | Meaning |
|---|---|---|
| `rank` | int | from 1 |
| `team_id`, `name` | string | the team |
| `bosses_defeated` | int | first key |
| `last_defeat_at` | string or null | when its latest boss fell: second key |
| `damage` | int | on its current boss: third key |
| `finished` | bool | `FINISHED` |

## The write-through
Doesn't apply: the leaderboard writes nothing.

## API
| Method | Path | Response |
|---|---|---|
| GET | `/api/leaderboard` | `200` `{top, teams, me}` |

```json
{"success": true, "payload": {
  "top": [
    {"rank": 1, "team_id": "482913", "name": "Scanners", "bosses_defeated": 6, "last_defeat_at": "2026-10-21T14:02:11Z", "damage": 0, "finished": true},
    {"rank": 2, "team_id": "120034", "name": "Runners", "bosses_defeated": 4, "last_defeat_at": "2026-10-21T13:40:02Z", "damage": 220, "finished": false}
  ],
  "teams": 140,
  "me": {"rank": 37, "team_id": "551200", "name": "Mine", "bosses_defeated": 2, "last_defeat_at": "2026-10-21T13:10:45Z", "damage": 90, "finished": false}
}}
```
`401 missing_token` / `invalid_token`. `me` is null without a started team.

**Event** `leaderboard` (game scope, every stream): `{"type": "snapshot" | "updated", "message": {"top": […], "teams": 140}}`. `snapshot` opens every stream (after `hello`, the user's `team`/`no_team` and `team_state`); `updated` when the top changed, at most once per **5 s** (`leaderboard.publish_every`).

## Rules and cases
| Case | Result |
|---|---|
| a lobby team | not ranked |
| a started team that did nothing | ranked with zeros, after every team with progress |
| equal bosses | earlier latest defeat first; a team with none after |
| equal bosses and time (or none) | more damage first, then team ID |
| a deleted team | gone from the ranking (teams and teamstate forget it) |
| top unchanged at a tick | nothing broadcast |
| a change below the top | nothing broadcast (GET shows it) |
| fewer teams than `size` | the top is all of them |

## Concurrency and locking
`rank()` reads each team's snapshot lock-free, one by one (teamstate's `Standings` holds its index read lock while it copies), so a ranking reads every team as it was at some moment, not all at the same instant: a cast during a ranking may or may not be in it, and the next tick catches it. `Refresh` holds `mu` while it compares and broadcasts, so two refreshes never interleave events.

## Failure and recovery
Nothing to fail but encoding (logged, nothing broadcast). A restart: `last` is empty, so the first tick broadcasts.

## Limits and how to change them
| Limit | Where | Change |
|---|---|---|
| Teams in the top: **10** | `leaderboard.size` (at least 1) | Edit, restart |
| Most often broadcast: every **5 s** | `leaderboard.publish_every` (at least 1 s) | Edit, restart |

## Not decided yet
- **Ties share nothing.** Two teams with the same progress get different ranks by team ID.
- **No per-team history** (rank over time).

## Tests
| Test | File | Covers |
|---|---|---|
| `TestRanking` | `tests/leaderboard_test.go` | every key and tie-break, ranks from 1, finished, lobby left out, a team with no standing, the top cut at size, the caller's entry below the top, null without a team (fakes) |
| `TestRefreshBroadcastsOnChange` | `tests/leaderboard_test.go` | the first refresh broadcasts `updated`; unchanged and below-the-top changes don't; a change in the top does; `Snapshot` is a `snapshot` |

End to end in `internal/server/tests`: `TestLeaderboardAndFinish` (the route, the finished team first, `me`, the stream's opening snapshot, a refresh's broadcast); the game-loop simulation's `checkLeaderboard` compares every team's rank with its model (teamstate README, Tests).
