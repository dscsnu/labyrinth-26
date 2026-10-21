# Admin: the organisers' toolbox

Every `/api/admin/…` route, in one use case. Admin owns **no data and no state**: each route checks its input and calls the use case that owns the data (`teams`, `teamstate`, `user`) through small interfaces declared here. It's a **facade** (AGENTS.md §3): `doc.go`, `api.go`, `read.go`, `write.go` and this README, but no `db.go` or `store.go`.

Config values appear with their key: **20** (`admin.user_search_limit`).

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
| **Admin** | A user whose role is `ADMIN` **in Postgres now**. `server.Build` wraps every admin route as `authAPI.RequireAuth(perUser(userAPI.RequireAdmin(h)))` (`perUser` is the per-user rate limit): `RequireAdmin` reads the role with `user.Get` on every request, never from the token (which can be up to **12 h**, `auth.token_ttl`, stale). Anyone else gets `403 not_admin`. |
| **Facade** | A use case that owns nothing: its writes are the owners' writes (their rules, write-through and events), run for an admin. Admin adds only rules about *who is acting*. |
| **Team overview** | One row of the admin's teams list: the team and, for a started or finished team, its **game at a glance** (health, knocked out, boss, cooldowns, cast count). |
| **Own role** | An admin can't change their own role (`409 cannot_change_own_role`), so nobody locks themselves out; another admin can. |

## The flow
1. **Look:** `GET /api/admin/teams` lists every team with its game at a glance (`/admin` filters to knocked-out teams).
2. **Act on a team:** revive it (health back, a fresh attack clock), reset its cooldowns, remove a member (naming a new leader when removing the leader), or delete it with all its progress.
3. **Act on a user:** find them by email (`GET /api/admin/users?email=`), then give them `PLAYER`, `HELPER` or `ADMIN`.

**Why one use case for every admin route.** The routes were spread over `teams` and `teamstate` (each had an `AdminRoutes`). One place makes the admin surface easy to audit, gives it one error table and one log line shape, and lets a route combine use cases (the overview joins teams with team state) without either importing the other (AGENTS.md §4).

**Why no data of its own.** Each admin write already has an owner with its own lock, write-through and events. A copy of any of that here would drift.

## How each file works
### `doc.go`
The package comment.

### `read.go`
- **Interfaces** (AGENTS.md §4), filled by `server.Build` with the real stores:
  - `Teams`: `All`, `ByID`, `RemoveMember`, `Delete` (`*teams.Store`)
  - `TeamState`: `TeamView`, `Revive`, `ResetCooldowns` (`*teamstate.Store`)
  - `Users`: `Search`, `SetRole` (`*user.Users`)
- **`Rules`**: `UserSearchLimit` (`admin.user_search_limit`).
- **`Admin`**, **`New(t, st, u, rules)`**.
- **`TeamOverview`** `{team, game}` and **`GameOverview`** (see [Data model](#data-model)).
- **`Teams()`**: `teams.All()` in ID order; for each team not in the `LOBBY`, `TeamState.TeamView(id)` copied into a `GameOverview`. All from memory.
- **`SearchUsers(ctx, query)`**: `Users.Search(ctx, query, UserSearchLimit)` (one Postgres query in `user`).

### `write.go`
Each write is one call to the owner:

| Function | Calls | Adds |
|---|---|---|
| `RemoveMember(ctx, teamID, userID, newLeader)` | `teams.RemoveMember` | nothing |
| `DeleteTeam(ctx, teamID)` | `teams.Delete` | nothing |
| `Revive(ctx, teamID)` | `teamstate.Revive` | nothing |
| `ResetCooldowns(ctx, teamID)` | `teamstate.ResetCooldowns` | nothing |
| `SetRole(ctx, adminID, userID, role)` | `user.SetRole` | `ErrCannotChangeOwnRole` when `adminID == userID`, checked first (the user use case isn't called) |

### `api.go`
- **`Routes(mux, admin)`** registers the seven routes behind `admin`.
- **`errorCodes`**: one table for every error the routes can meet, admin's and the owners':

  | Error | Status | Code |
  |---|---|---|
  | `teams.ErrTeamNotFound` | 404 | `team_not_found` |
  | `teams.ErrNotMember` | 404 | `not_a_member` |
  | `teams.ErrLastMember` | 409 | `last_member` |
  | `teams.ErrNewLeaderRequired` | 409 | `new_leader_required` |
  | `teams.ErrInvalidNewLeader` | 409 | `invalid_new_leader` |
  | `teamstate.ErrNotInGame` | 409 | `not_in_game` |
  | `user.ErrUserNotFound` | 404 | `user_not_found` (401 on `GET /api/me`, where it's the caller's own token) |
  | `user.ErrInvalidRole` | 400 | `invalid_role` |
  | `ErrCannotChangeOwnRole` | 409 | `cannot_change_own_role` |

- **`teamID(w, r)`**: `{team_id}` must match `teamIDPattern` (6 digits, teams' format), else `400 invalid_team_id`. Every team route checks it first.
- **`userID(w, raw, what)`**: a UUID, else `400 invalid_user_id` naming the field.
- Handlers `teams`, `deleteTeam`, `removeMember`, `revive`, `resetCooldowns`, `searchUsers`, `setRole`. **Every write is logged** at info with `admin_id` and what it acted on.

## Data model
**Memory:** none. **Postgres:** none of its own; the owners' tables (see their READMEs).

`GameOverview` (JSON), the same values as the team's `team_state`:

| Field | Type | Meaning |
|---|---|---|
| `health` | int | the team's health |
| `knocked_out` | bool | health 0: it needs a revive |
| `bosses_defeated` | int | how many bosses it has defeated |
| `boss` | object or null | as `team_state.boss` (serial, damage, phase start, attacks, next attack); null after the last |
| `cooldown_class` | string or null | the spell class it can't cast now |
| `qr_cooldowns` | array | the waypoints it can't scan yet, each with `until` |
| `cast_count` | int | how many spells it has cast |

## The write-through
Doesn't apply: admin persists nothing. Each owner runs its own write-through (teams' `RemoveMember` and `Delete`, teamstate's `Revive` and `ResetCooldowns`; user's `SetRole` writes Postgres directly).

## API
| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/api/admin/teams` | none | `200` `[TeamOverview]`, by team ID |
| DELETE | `/api/admin/teams/{team_id}` | none | `200`, no payload |
| DELETE | `/api/admin/teams/{team_id}/members/{user_id}[?new_leader=<user id>]` | none | `200` the team after the removal |
| POST | `/api/admin/teams/{team_id}/revive` | none | `200` the team's state |
| POST | `/api/admin/teams/{team_id}/cooldowns/reset` | none | `200` the team's state |
| GET | `/api/admin/users[?email=<part>]` | none | `200` `[User]`, at most **20** (`admin.user_search_limit`), by email |
| PUT | `/api/admin/users/{user_id}/role` | `{"role": "PLAYER" \| "HELPER" \| "ADMIN"}` | `200` the user |

Every route: `401 missing_token` / `invalid_token` / `user_not_found` (the caller's own account), `403 not_admin`.

`GET /api/admin/teams`:
```json
{"success": true, "payload": [
  {"team": {"id": "120034", "name": "Lobby", "status": "LOBBY", "leader_id": "…", "members": […], "version": {…}}, "game": null},
  {"team": {"id": "482913", "name": "Scanners", "status": "IN_GAME", "leader_id": "…", "members": […], "version": {…}},
   "game": {"health": 0, "knocked_out": true, "bosses_defeated": 2,
            "boss": {"serial": 3, "damage": 107, "phase_started_at": "2026-10-21T12:00:00Z", "attacks": 4, "next_attack_at": null},
            "cooldown_class": "class_4", "qr_cooldowns": [{"waypoint_id": "2001", "until": "2026-10-21T12:44:00Z"}], "cast_count": 7}}
]}
```

`PUT /api/admin/users/9a2b…/role` `{"role": "HELPER"}` → `{"success": true, "payload": {"id": "9a2b…", "email": "…", "role": "HELPER", …}}`.

Events: whatever the owner publishes (see [`internal/server/README.md`, "What publishes what"](../server/README.md#what-publishes-what)): removal and deletion publish `team` / `no_team`; revive and reset publish `team_state` (`revived`, `cooldowns_reset`); a role change publishes nothing.

## Rules and cases
In validation order, per route (after the guard: `401`, then `403 not_admin`).

**Team routes** (`DELETE …/{team_id}`, `…/members/{user_id}`, `…/revive`, `…/cooldowns/reset`):

| Case | Result |
|---|---|
| `team_id` isn't 6 digits | `400 invalid_team_id` |
| member route: `user_id` or `new_leader` isn't a UUID | `400 invalid_user_id` |
| no such team | `404 team_not_found` |
| revive / reset on a team not `IN_GAME` | `409 not_in_game` |
| removal rules | as `teams.RemoveMember`: `not_a_member`, `last_member`, `new_leader_required`, `invalid_new_leader` |
| delete | always allowed, any status; the members get `no_team` (`team_deleted`) |

**Users:**

| Case | Result |
|---|---|
| search with no `email` | the first **20** users by email |
| `%` or `_` in `email` | matched literally |
| role: `user_id` not a UUID | `400 invalid_user_id` |
| role: body not `{"role": string}` or empty role | `400 invalid_body` |
| role: the admin's own ID | `409 cannot_change_own_role` (nothing changed) |
| role: not `PLAYER` / `HELPER` / `ADMIN` | `400 invalid_role` |
| role: no such user | `404 user_not_found` |
| role: set | applies to `RequireAdmin` / `RequireStaff` at once; the token's copy updates at the next sign-in |

## Concurrency and locking
Admin holds no locks. Every race is the owner's, and its README says who wins: a removal against a scan (teams), a deletion against a write in flight (teamstate's `TeamDeleted`), a revive or reset against a timer (teamstate, under the team's entry lock). Two admins changing the same role: the last write wins.

## Failure and recovery
Nothing to recover: a failed owner write reconciles in the owner. Postgres down: `RequireAdmin` can't read the role, so every admin route is `500` (logged), and nothing is changed.

## Limits and how to change them
| Limit | Where | Change |
|---|---|---|
| Users per search: **20** | `admin.user_search_limit` | Edit, restart |

## Not decided yet
- **No audit trail beyond logs.** Admin writes are logged with `admin_id`, not stored.
- **No undo** for a deleted team.
- **No adding a player to a started team.**

## Tests
| Test | File | Covers |
|---|---|---|
| `TestTeamsOverview` | `tests/admin_test.go` | a lobby team's game is null; a started team's comes from its team view (fakes) |
| `TestSetRoleNotOwn` | `tests/admin_test.go` | an admin's own role is refused without calling the user use case; someone else's is set |
| `TestSearchLimit` | `tests/admin_test.go` | a search passes `admin.user_search_limit` |

End to end over HTTP, against the real use cases, in `internal/server/tests/admin_test.go`: `TestAdminGuard`, `TestAdminRemoveFromLobby`, `TestAdminRemoveFromStartedTeam`, `TestAdminRemoveRacesScan`, `TestAdminRevive`, `TestAdminTeamsOverview`, `TestAdminDeleteTeam`, `TestAdminResetCooldownsOverHTTP`, `TestAdminUsersAndRoles` (see the server README's Tests).
