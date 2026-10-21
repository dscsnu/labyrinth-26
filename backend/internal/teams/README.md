# teams

Teams of up to **6** (`teams.size`) players with a leader, from forming in the lobby to starting the game, plus an admin's removal of a member at any time.

Every number below that comes from `config.toml` is shown with its current value and its key, e.g. **6** (`teams.size`). Change the file, not the code.

## Contents
1. [Concepts](#concepts)
2. [The flow](#the-flow)
3. [How each file works](#how-each-file-works)
4. [Data model: memory](#data-model-memory)
5. [Data model: Postgres](#data-model-postgres)
6. [Memory ↔ Postgres](#memory--postgres)
7. [Walkthrough: forming and starting a team](#walkthrough-forming-and-starting-a-team)
8. [The write-through](#the-write-through)
9. [API](#api)
10. [Rules and cases](#rules-and-cases)
11. [Concurrency and locking](#concurrency-and-locking)
12. [Failure and recovery](#failure-and-recovery)
13. [Limits and how to change them](#limits-and-how-to-change-them)
14. [Not decided yet](#not-decided-yet)
15. [Tests](#tests)

## Concepts
| Term | Meaning |
|---|---|
| **Team** | Up to **6** (`teams.size`) players, identified by a random 6-digit **code** (`"482913"`) that players type to join. |
| **Leader** | The creator. Starts the game. Doesn't ready up. If the leader leaves the lobby, the longest-standing member takes over; if an admin removes the leader, the admin names the new one. |
| **Status** | `LOBBY` (forming) → `IN_GAME` (started; locked to players: nobody joins or leaves) → `FINISHED` (it defeated the last boss: `Finish`, called by teamstate; its game writes are `not_in_game`, and its members stay in it, so they can't join another team). |
| **Admin removal** | An `ADMIN` (role read from the database on each request) removes a member from a team in any status. The only way a started team's roster changes, and it can only shrink. |
| **Ready** | A non-leader member's "I'm set" flag. The leader can start once every other member is ready, with any number of members from 1 (the leader alone) to **6** (`teams.size`). |
| **Join order** | Members are kept in the order they joined. It decides who becomes leader next. |

## The flow
1. **Create.** A signed-in player with no team sends `POST /api/teams {"name"}`. They become the leader and only member of a new `LOBBY` team with a random 6-digit code.
2. **Share the code.** The leader tells teammates the code (out of band: in person, chat).
3. **Join.** Each teammate sends `POST /api/teams/join {"team_id"}` until the team has **6** (`teams.size`). Members are kept in join order.
4. **Ready.** Each non-leader sends `PUT /api/teams/me/ready {"ready": true}`. They can un-ready, leave and rejoin while in the lobby.
5. **Start.** The leader sends `POST /api/teams/me/start`. With any number of members (1 to **6**, `teams.size`), all non-leaders ready, the team becomes `IN_GAME` and its roster is locked to players (only an admin can remove a member, step 7). From here `teamstate` takes over: right after the `IN_GAME` `team` event, every member gets the team's (empty) `team_state` through `Rules.OnStart`.
6. **Live updates.** Every member's event stream (`GET /api/events`) gets the whole team after every change, and `no_team` when they leave.
7. **Admin removal (any time).** An admin sends `DELETE /api/admin/teams/{team_id}/members/{user_id}` (a route of the `admin` use case, which calls `RemoveMember`), adding `?new_leader=<user id>` when removing the leader. The removed player gets `no_team` and is free to create or join a team; their past scans and casts stay with the team. A started team keeps playing with the rest. For a started team, teams then calls `Rules.OnRosterChange`, so teamstate stops sending to the removed player and refuses their writes.
8. **Finish.** When a cast defeats the team's last boss, teamstate calls `Rules.OnFinished`, which `server.Build` wires to `Finish`: the team becomes `FINISHED` (team event `finished`). Its game writes are refused (`not_in_game`); its members stay in it.
9. **Admin deletion (any time).** `DELETE /api/admin/teams/{team_id}` (admin use case) calls `Delete`: the team and all its rows go (members and progress, by `ON DELETE CASCADE`), every member gets `no_team` (`team_deleted`), and `Rules.OnDelete` tells teamstate to forget the team.

- **Why the leader doesn't ready up.** Starting is the leader's "ready". A separate flag would only add a state that can disagree with pressing start.
- **Why join order decides the next leader.** It's stable, visible to everyone, and survives a restart (`joined_at`, ties broken by `user_id`).
- **Why players can't join or leave a started team.** A team's game is the team's: a player hopping teams mid-game would carry nothing and confuse everyone. So once `IN_GAME`, only an admin can change the roster, and only by removing someone (someone went home, or was put in the wrong team).
- **Why admin removal can't empty a team.** Removing the last member would delete the team, and a started team's progress with it (`team_scan`, `team_cast` cascade). Deleting a team is a separate, explicit action (`Delete`, `DELETE /api/admin/teams/{team_id}`); `last_member` refuses doing it by accident.
- **Why the admin names the new leader.** The admin knows who is still there; join order (what `Leave` uses) may pick someone who also went home.
- **Why `OnRosterChange` exists.** teamstate reads a started team without teams' lock, caches its roster for publishing, and accepts writes from whoever the roster says. When an admin removes someone it must drop that cache and stop accepting their writes; teams can't import teamstate (AGENTS.md §4), so `server.Build` wires it like `OnStart`.
- **Why `Finish` is called by teamstate, and outside its lock.** Only teamstate knows when the last boss falls, but `FINISHED` is a team status, owned here. teamstate calls `OnFinished` after releasing its team lock: teams' writes call back into teamstate (`OnRosterChange`, `OnDelete`) while holding the team's lock, so taking the locks the other way round could deadlock.
- **Why a finished team's members can't join another.** They're still in it (`userTeam`), so `Create` and `Join` say `already_in_team`: a team's result stays its own.
- **Why `OnDelete` exists.** A deleted team's rows vanish from Postgres by cascade, but teamstate still holds its state and its timer in memory; `OnDelete` (wired to `teamstate.TeamDeleted`) drops them.
- **Why `OnStart` is a callback.** A start must also send the team's `team_state` (AGENTS.md §17: everything about the game loop reaches clients through it), but teams may not import teamstate (AGENTS.md §4: teamstate imports teams). So `server.Build` passes `OnStart`, the same way it passes auth's `onSignIn` → `RefreshProfile`.

## How each file works

### `api.go`: routes
- **`Routes`** registers six routes behind the auth middleware. The live stream is registered by `internal/server`, because it carries events from several use cases.
- **No admin routes here.** Every `/api/admin/…` route lives in the `admin` use case ([`internal/admin/README.md`](../admin/README.md)), which calls `All`, `RemoveMember` and `Delete`.
- Each handler decodes and checks the body, calls one `Store` method with the caller's ID (`identity.UserID`), and writes the result in the envelope.
- **`create`** trims the name (`strings.TrimSpace`); then **`validName`** enforces 1 to **32** (`teams.name_max_length`) characters (counted as runes) with no control characters. **`teamIDPattern`** requires exactly 6 digits.
- **`fail`** maps rule errors to a status and code via `errorCodes`; anything else is logged and becomes `500`.

### `read.go`: types and reads
- **Types:** `Team`, `Member`, `Status` (`Lobby`, `InGame`, `Finished`), and the error `ErrNotInTeam`.
- **Reads**, from memory only:
  - `ForUser(userID)`: two map lookups under a brief shared lock, then a lock-free snapshot load. About 60 ns per read, measured with `BenchmarkForUser` (one team of **6** (`teams.size`) members, 16 goroutines reading at once, on an i7-12650H)
  - `ByID(teamID)`
  - `All()`: every team, sorted by ID. Copies the index under the shared lock, then loads each snapshot lock-free. For the admin use case (its teams list) and the leaderboard (every ranked team); the restart test compares `All()` (plus each member's `ForUser`) before and after.
  - `Snapshot(userID)`: the `team`/`no_team` event of type `snapshot` for a new stream
  - `Store.Size()`: the method returning **6** (`teams.size`), the team size from `Rules`. Used by tests.

### `write.go`: writes, each written through
- **`Create`**, **`Join`**, **`Leave`**, **`SetReady`**, **`Start`**, **`RemoveMember`**, **`Finish`** and **`Delete`** each follow the [write-through](#the-write-through) steps, numbered in the code.
- **`Start`**, after step 6 publishes the `IN_GAME` `team`, calls **`Rules.OnStart(team)`** if it's set, still holding the team's entry lock, so each member's stream gets `team` before whatever the hook publishes. `server.Build` sets it to `teamstate.Store.PublishTeam`, which publishes the team's `team_state`. A rejected start, or one whose persist fails, never calls it.
- **`RemoveMember(ctx, teamID, userID, newLeader)`** is the admin's removal. Locks the team by ID (`lockTeam`), validates (member? last member? new leader required, a remaining member, and only when removing the leader), builds the team without the member (and with the new leader), persists with `removeMember(…, deleteTeam=false)`, applies (drops the user from `userTeam`), publishes `team` to the rest and `no_team` to the removed user, then, if the team isn't `LOBBY`, calls **`Rules.OnRosterChange(team)`** still holding the lock. Returns the team after the removal.
- **`Finish(ctx, teamID)`** marks a team `FINISHED`: called through teamstate's `Rules.OnFinished` (without teamstate's lock) when a cast defeats the last boss, and again at teamstate's `Load` for a team found with every boss defeated but still `IN_GAME`. Locks the team by ID; a no-op unless it's `IN_GAME` (so a repeat changes nothing); builds the `FINISHED` copy with the next version; persists with `updateFinished`; publishes `team` of type `finished`. `ErrTeamNotFound` for an unknown team.
- **`Delete(ctx, teamID)`** is the admin's deletion, any status. Locks the team by ID; persists with `deleteTeam` (the cascade removes every member and progress row); applies (drops every member from `userTeam` and the team from the index, marks the entry gone); publishes `no_team` (`team_deleted`) to every member; then calls **`Rules.OnDelete(teamID)`** still holding the lock. `ErrTeamNotFound` for an unknown team.
- **`RefreshProfile(userID, name, avatarURL)`** updates a member's `Name` and `AvatarURL` in memory and publishes `team`. Called by auth's sign-in hook (wired in `server.Build`) after every successful sign-in, with the user as `userprofile` now stores it. Steps: 1 lock (`lockUsersTeam`), 2 validate (anything changed?), 3 build (clone `Members`, set the two fields), 5 apply, 6 publish. **No persist:** the user use case has already saved `userprofile`, and teams reads names and avatars from it at load. A no-op when the user is in no team or nothing changed. Returns nothing.
- **`equalPtr(a, b)`**: both `nil`, or both pointing to equal strings. Compares avatars.
- The rule errors are declared here.

### `db.go`: SQL
- **`selectAll`** reads every team and every member, in join order. `selectOne` / `selectTeam` read one.
- **`insertTeam`**: one transaction that inserts `team` (`ON CONFLICT (id) DO NOTHING`, giving `errTeamIDTaken` on a collision), inserts the leader into `teammember`, and reads the team back.
- **`insertMember`**: inserts and returns the member joined with their profile, in one statement (`WITH ins AS (…)`).
- **`removeMember`**: one transaction that deletes the member, moves the leader if needed, and deletes the team if it's now empty.
- **`updateReady`** and **`updateStarted`**. `updateStarted` returns Postgres's `now()` and only matches a `LOBBY` team.
- **`updateFinished`**: `UPDATE team SET status = 'FINISHED' WHERE id = $1 AND status = 'IN_GAME'` (`execOne`).
- **`deleteTeam`**: `DELETE FROM team WHERE id = $1` (`execOne`); `ON DELETE CASCADE` removes its `teammember` rows and every teamstate row (`team_scan`, `team_pattern`, `team_selection`, `team_cast`, `team_boss`).
- **Helpers:**
  - `execOne` fails unless exactly one row changed, which is how a memory/database disagreement is detected.
  - `mapMemberInsertErr` maps the `teammember_user_id_key` violation to `errUserInTeam`.

### `store.go`: the in-memory layout
- **`Rules`**: the settings `server.Build` passes in. `Size` (`teams.size`), `NameMaxLength` (`teams.name_max_length`), `WriteTimeout` (`storage.write_timeout`), `CodeAttempts` (`teams.code_attempts`), and three hooks not from config.toml: `OnStart func(Team)`, the start hook; `OnRosterChange func(Team)`, called by `RemoveMember` for a started team; and `OnDelete func(teamID string)`, called by `Delete` (see `write.go`). `nil` means no hook (the teams tests, `cmd/loadtest`). `server.Build` wires them to teamstate's `PublishTeam`, `RosterChanged` and `TeamDeleted`.
- **`Load`** builds memory at startup.
- **`persist`** runs a write detached from the request, capped at **5 s** (`storage.write_timeout`).
- **`lockUsersTeam`** finds and locks a user's team, retrying if the user moved team between lookup and lock.
- **`lockTeam`** finds and locks a team by ID (`RemoveMember`): `ErrTeamNotFound` if there's none, or it was deleted (`gone`) while waiting for the lock.
- **`reconcile`** reloads a team after a failed write.
- **`publish(t, typ)`** sends `team` of type `typ` to the members; **`sendNoTeam(ev, users...)`** sends a prebuilt `no_team` event (one per type, `noTeamLeft`, `noTeamDeleted`, `noTeamRemoved`, `noTeamResync`, `noTeamSnapshot`, encoded once since `{}` never changes). **`teamEvent(t, typ)`** builds the envelope. The type constants (`TeamCreated` … `TeamProfileUpdated`, `NoTeamLeft`, `NoTeamDeleted`, `NoTeamRemoved`) are in `store.go`; `live.Snapshot` and `live.Resync` come from `platform/live`.
- **Version:** `Rules.Epoch` (one per process, from `server.Build`). Every write's build step sets `next.Version = t.Version.Next(epoch)` (`Create` starts at seq 1); `reconcile` continues from the in-memory version; `Load` starts every team at seq 0. No-ops don't bump. **`randomTeamID`** picks codes from 100000–999999.

## Data model: memory
```
Store
  mu        sync.RWMutex              guards the two maps; never held during I/O
  teams     map[teamID]*entry         "give me that team"
  userTeam  map[userID]teamID         "which team is this user in?"
  hub       *live.Hub                 where events go

entry
  mu    sync.Mutex                    writers to this one team queue here
  cur   atomic.Pointer[Team]          current snapshot; readers load it lock-free
  gone  bool                          team deleted; guarded by mu

Team                                  (never modified once stored)
  ID         string                   6 digits
  Name       string                   1 to 32 (teams.name_max_length) characters
  LeaderID   uuid.UUID
  Status     Status                   LOBBY | IN_GAME | FINISHED
  CreatedAt  time.Time
  StartedAt  *time.Time               nil until started
  Members    []Member                 join order; ≤ 6 (teams.size); copied, never edited in place

Member
  ID, Name, AvatarURL, IsReady, JoinedAt
```
- **Snapshots are copy-on-write.** A write copies the `Team` (and `slices.Clone`s `Members` if it changes them), then swaps the pointer. A reader holding the old pointer keeps a consistent old view.
- **`gone`** tells a writer who was waiting on the lock that the team was deleted meanwhile.
- **Memory:** about 1 KB per team with **6** (`teams.size`) members (an estimate, not measured).

## Data model: Postgres
Migration `0002`:

| Table | Columns | Constraints and what they back |
|---|---|---|
| `team` | `id` VARCHAR(6), `name`, `leader_id`, `status`, `created_at`, `started_at` | `id ~ '^[0-9]{6}$'`: codes are 6 digits. `char_length(name) BETWEEN 1 AND 32`. `leader_id` → `userprofile` |
| `teammember` | `team_id`, `user_id`, `is_ready`, `joined_at` | PK `(team_id, user_id)`. **UNIQUE `user_id`: one team per user**, so a concurrent create/join race can't put a user in two teams. `team_id` → `team` `ON DELETE CASCADE` |

## Memory ↔ Postgres
| Postgres | Memory |
|---|---|
| `team` row | `Team` fields |
| `teammember` rows joined with `userprofile` (name, avatar), ordered by `joined_at, user_id` | `Team.Members`, and `userTeam[user] = team` |
| `userprofile.name`, `avatar_url` changed by a sign-in | `Member.Name`, `Member.AvatarURL`, via `RefreshProfile` at once; nothing teams writes |
| (none: memory-only) | `Team.Version`: `{epoch, seq}` for clients to order snapshots. `Load` starts every team at seq 0 in the new process's epoch, which clients treat as newer than anything from the old process, so nothing needs storing |

Ties in `joined_at` break by `user_id`, so the order (and so leader succession) is the same after a restart. Names and avatars match too: `RefreshProfile` only copies into memory what `userprofile` already holds, so a restart reads the same values.

## Walkthrough: forming and starting a team
Players A–F: a full team of **6** (`teams.size`), config.toml as shipped.

| Step | Request | Memory after | Postgres after | Events |
|---|---|---|---|---|
| 1 | A: `POST /api/teams {"name":"Seven Evil Exes"}` | `teams["482913"]` → `Team{Leader A, LOBBY, Members [A]}`; `userTeam[A]="482913"` | `team` row; `teammember(A, ready=false)` | A ← `team` |
| 2 | B–F: `POST /api/teams/join {"team_id":"482913"}` (each) | `Members` grows in join order; `userTeam[B..F]` set | `teammember` +5 | everyone in the team ← `team` after each join |
| 3 | A: `POST /api/teams/me/start` | unchanged | unchanged | none; A gets `409 not_all_ready` (6 members, but B–F aren't ready) |
| 4 | B–F: `PUT /api/teams/me/ready {"ready":true}` (each) | that member's `IsReady=true` | `teammember.is_ready=true` | all ← `team` |
| 5 | A: `POST /api/teams/me/start` | `Status IN_GAME`, `StartedAt` = Postgres `now()` | `team.status='IN_GAME', started_at` | all ← `team` |
| 6 | C: `POST /api/teams/leave` | unchanged | unchanged | `409 team_locked` |

Had A left at step 2 (lobby, B–F present), B, the earliest joiner, would have become leader in memory and in `team.leader_id`, in the same transaction as removing A. A would get `no_team`; B–F would get the team with `leader_id = B`.

## The write-through
Every function in `write.go`, marked in the code:
1. **lock** the team's entry (`lockUsersTeam`, or the entry by code for `Join`, `RemoveMember`, `Finish` and `Delete`)
2. **validate** the rules against the snapshot
3. **build** the next snapshot
4. **persist** via `db.go`, detached from the request, capped at **5 s** (`storage.write_timeout`)
5. **apply**: swap the snapshot, update `teams`/`userTeam`
6. **publish** `team` to members (`no_team` to a leaver), each with its envelope type (below); the build step set the next `Version`

| Function | Steps, in code order | `db.go` call | Why the order differs (AGENTS.md §6) |
|---|---|---|---|
| `Create` | 2, 4, 3, 5, 6 (no 1) | `insertTeam` | (c) a new team has no entry to lock until apply makes it; the unique constraint on `teammember.user_id` guards a concurrent create or join. (b) Postgres sets `created_at` and `joined_at`, so the snapshot is the team it returns |
| `Join` | 1, 2, 4, 3, 5, 6 | `insertMember` | (b) Postgres sets `joined_at` and returns the member with their profile |
| `Leave` | 1–6 | `removeMember` | |
| `SetReady` | 1–6 | `updateReady` | |
| `Start` | 1, 2, 4, 3, 5, 6 (6 also calls `Rules.OnStart`) | `updateStarted` | (b) Postgres sets `started_at` |
| `RemoveMember` | 1–6 (6 also calls `Rules.OnRosterChange` for a started team) | `removeMember` (never deletes the team) | |
| `Finish` | 1–6 (a no-op unless `IN_GAME`) | `updateFinished` | |
| `Delete` | 1, 4, 5, 6 (6 also calls `Rules.OnDelete`) | `deleteTeam` | nothing to validate or build: the team goes |
| `RefreshProfile` | 1, 2, 3, 5, 6 (no 4) | none | memory-only: `user` already saved `userprofile` |

**On a failed persist:** `reconcile` reloads the team from Postgres, re-indexes its members, publishes, and the caller gets `500`. **SetReady** to the current value returns before step 4. **RefreshProfile** skips step 4 entirely (the data is already in `userprofile`), so it can't fail and never reconciles.

## API
All routes need `Authorization: Bearer <token>` (`401` without). The admin routes that act on teams (list, remove a member, delete) are the `admin` use case's ([`internal/admin/README.md`](../admin/README.md)); the cases below for removal and deletion are this use case's rules, which they call.

| Method | Path | Body | Success |
|---|---|---|---|
| GET | `/api/teams/me` | none | `200` team |
| POST | `/api/teams` | `{"name": "Seven Evil Exes"}` | `201` team |
| POST | `/api/teams/join` | `{"team_id": "482913"}` | `200` team |
| POST | `/api/teams/leave` | none | `200`, no payload |
| PUT | `/api/teams/me/ready` | `{"ready": true}` | `200` team |
| POST | `/api/teams/me/start` | none | `200` team |

**Team JSON** (payload, and the `team` event's `message`):
```json
{"id": "482913", "name": "Seven Evil Exes",
 "leader_id": "6f1c…", "status": "LOBBY",
 "created_at": "2026-10-17T12:00:00Z", "started_at": null,
 "members": [
   {"id": "6f1c…", "name": "Jia Khot", "avatar_url": "https://…", "is_ready": false, "joined_at": "2026-10-17T12:00:00Z"},
   {"id": "9a2b…", "name": "Aarav", "avatar_url": null, "is_ready": true, "joined_at": "2026-10-17T12:00:04Z"}
 ],
 "version": {"epoch": 1760702400000, "seq": 2}}
```
`version` orders this team's snapshots: keep a copy only if its `epoch` differs from the one held, or its `seq` is higher. Every response above that returns a team carries it, like the event.
Emails are never included. **Error body:** `{"success": false, "code": "team_full", "message": "team is full"}`.

**Live events** on `GET /api/events` (team scope, sent to each member with `Hub.Send`), each data an envelope `{"type", "message"}`: `team` (message: the JSON above), or `no_team` (message: `{}`). The type says why:

| Write | `team` type (to the members) | `no_team` type (to the user out) |
|---|---|---|
| connect | `snapshot` | `snapshot` (user in no team) |
| `Create` | `created` | |
| `Join` | `member_joined` | |
| `Leave`, others remain | `member_left`, or `leader_changed` if the leader left | `left` |
| `Leave`, last member | | `team_deleted` |
| `SetReady` | `ready_changed` | |
| `Start` | `started` (then teamstate's `team_state` of type `game_started`, through `Rules.OnStart`) | |
| `RemoveMember` (admin) | `member_removed`, or `leader_changed` if the leader was removed | `removed` (and from then on no `team_state`: teamstate's `RosterChanged`) |
| `Finish` (the last boss fell) | `finished` | |
| `Delete` (admin) | | `team_deleted` to every member (and teamstate forgets the team: `TeamDeleted`) |
| `RefreshProfile` | `profile_updated` | |
| `reconcile` | `resync` | `resync` |

Every type and when it's sent: `internal/server/README.md`, "Event format".

## Rules and cases

### Create: `POST /api/teams`
| Case | Result |
|---|---|
| Body isn't `{"name": string}` or has unknown fields | `400 invalid_body` |
| Name empty after trimming, over **32** (`teams.name_max_length`) characters, or contains a control character | `400 invalid_team_name` |
| Name with emoji or accents, ≤ **32** (`teams.name_max_length`) characters | Allowed (counted in characters, not bytes) |
| Caller already in a team | `409 already_in_team` |
| Caller creates and joins at the same moment | One wins; the other gets `already_in_team` (unique constraint) |
| Random code already taken | Another is tried (up to **10** (`teams.code_attempts`), then `500`) |
| Success | `201`; caller is leader and only member, `LOBBY`; `team` to the caller |

### Join: `POST /api/teams/join`
Checked in this order:

| Case | Result |
|---|---|
| Body isn't `{"team_id": string}` | `400 invalid_body` |
| `team_id` not exactly 6 digits | `400 invalid_team_id` |
| No such team, or deleted while waiting for the lock | `404 team_not_found` |
| Team not `LOBBY` | `409 team_locked` |
| Caller already in a team, including this one | `409 already_in_team` |
| Team already has **6** (`teams.size`) | `409 team_full` |
| 2 × (`teams.size` − 1) = **10** people join a 1-member team at once | Exactly `teams.size` − 1 = **5** succeed; the rest get `team_full` |
| Success | Added last in join order, not ready; `team` to all members |

### Leave: `POST /api/teams/leave`
| Case | Result |
|---|---|
| Caller not in a team | `404 not_in_team` |
| Team not `LOBBY` | `409 team_locked` |
| A non-leader leaves | Removed; others keep their ready flags |
| The leader leaves, others remain | The earliest joiner becomes leader (same transaction); their ready flag stops mattering |
| The last member leaves | Team deleted; its code stops working (`team_not_found`) |
| Success | `200`; leaver gets `no_team`, remaining members get `team` |

### Ready: `PUT /api/teams/me/ready`
| Case | Result |
|---|---|
| Body missing `ready`, or not a boolean | `400 invalid_body` |
| Caller not in a team | `404 not_in_team` |
| Team not `LOBBY` | `409 team_locked` |
| Caller is the leader | `409 leader_cannot_ready` |
| Already at that value | `200`, nothing written, no event |
| Success | `200`; `team` to all members |

### Start: `POST /api/teams/me/start`
Checked in this order:

| Case | Result |
|---|---|
| Caller not in a team | `404 not_in_team` |
| Caller isn't the leader (even after the start) | `403 not_leader` |
| Team not `LOBBY` (already started) | `409 team_locked` |
| Any non-leader not ready | `409 not_all_ready` |
| Success | `IN_GAME`, `started_at` = Postgres `now()`; locked to players; `team` to all members, then `Rules.OnStart(team)` (in the server: `team_state` to all members) |

### Remove a member (admin): `RemoveMember`, behind `DELETE /api/admin/teams/{team_id}/members/{user_id}`
The admin use case checks the guard and the IDs first (`401`, `403 not_admin`, `400 invalid_team_id`, `400 invalid_user_id`); then, in this order:

| Case | Result |
|---|---|
| No such team, or deleted while waiting for the lock | `404 team_not_found` |
| User isn't in that team | `404 not_a_member` |
| User is the team's only member | `409 last_member` (with or without `new_leader`) |
| User leads the team, no `new_leader` | `409 new_leader_required` |
| User leads the team, `new_leader` is the user or not in the team | `409 invalid_new_leader` |
| User doesn't lead the team, `new_leader` given | `409 invalid_new_leader` |
| Success, any status (`LOBBY`, `IN_GAME`, `FINISHED`) | `200` with the team; member removed (others keep their ready flags), leader moved if named, in one transaction; the rest get `team`, the removed user `no_team`; logged with the admin's ID |
| Success, team started | Also `Rules.OnRosterChange(team)`: teamstate drops its cached roster and refuses the removed user's writes (`404 not_in_team`). Their scans and casts stay |
| After success | The removed user is teamless: they may create or join a (`LOBBY`) team |

### Finish: `Finish` (no route; teamstate's `OnFinished`)
| Case | Result |
|---|---|
| No such team (or deleted) | `ErrTeamNotFound` (logged by `server.Build`'s hook) |
| Team not `IN_GAME` (`LOBBY`, or already `FINISHED`) | No-op: nothing written or published |
| `IN_GAME` | `FINISHED`, stored; `team` of type `finished` to the members. From then on: teamstate's writes are `409 not_in_game`; `Create` and `Join` by a member are `409 already_in_team` (they're still in it) |
| Persist fails | `reconcile`, the error returned (and logged) |

### Delete (admin): `Delete`, behind `DELETE /api/admin/teams/{team_id}`
The admin use case checks the guard and `team_id` first; then:

| Case | Result |
|---|---|
| No such team, or deleted while waiting for the lock | `404 team_not_found` |
| Any status | The team, its members and all its progress deleted in Postgres (cascade); every member gets `no_team` (`team_deleted`) and is teamless; `Rules.OnDelete(teamID)` (teamstate's `TeamDeleted`). `200`, no payload |
| Deleting it again | `404 team_not_found` |

### Refresh a profile: `RefreshProfile` (no route; after each sign-in)
Checked in this order:

| Case | Result |
|---|---|
| User in no team | No-op |
| Name and avatar both unchanged (avatars compared by value; both `nil` counts as equal) | No-op: no event |
| Otherwise | Member's `name` and `avatar_url` replaced in memory; `team` to all members. Any status, including `IN_GAME` |
| Signed in without a picture | `user` keeps the stored avatar, so the avatar passed in is the old one: only the name can change |

### Get: `GET /api/teams/me`
| Case | Result |
|---|---|
| Caller not in a team | `404 not_in_team` |
| Otherwise | `200` with the team |

## Concurrency and locking
- **Writes to one team are serialised** on its `entry.mu`; different teams run in parallel.
- **Lock order:** `entry.mu`, then `Store.mu`, never the reverse. `Store.mu` is held only for map reads and writes, never during I/O.
- **2 × (`teams.size` − 1) = 10 joins to a 1-member team at once:** exactly `teams.size` − 1 = 5 succeed (`TestConcurrentJoinsRespectTeamSize`).
- **The leader starts while a member leaves:** either the start wins (the leave gets `team_locked`) or the leave wins and the smaller team starts; the team is always consistent (`TestConcurrentStartAndLeave`).
- **A user creates and joins at once:** the unique constraint on `teammember.user_id` lets only one through.
- **`lockUsersTeam`** re-checks membership after taking the lock, and retries up to 3 times if the user moved team.
- **An admin removal is a normal team write:** it takes the same `entry.mu`, so it's ordered with joins, leaves, readies and starts. A removal racing the user's own leave (lobby only): if the removal wins, the leave gets `not_in_team`; if the leave wins, the removal gets `not_a_member` (or `team_not_found` if the leave emptied the team).
- **The roster hook runs under the team's lock,** like `OnStart`, after apply: teamstate's `RosterChanged` takes teamstate's entry lock (same order: teams `entry.mu` → teamstate entry). A game write by the removed user either finishes before the hook gets teamstate's lock (it ran before the removal) or re-checks membership under that lock after the removal is visible and is refused (`TestAdminRemoveRacesScan`).
- **`Finish` runs without teamstate's lock.** teamstate calls `OnFinished` after releasing its own team lock, so `Finish` takes teams' `entry.mu` freely: the only cross-use-case lock order stays teams `entry.mu` → teamstate entry (the roster, delete and start hooks). A write racing it is ordered on `entry.mu` like any other.
- **The delete hook runs under the team's lock,** like the roster hook: `TeamDeleted` removes the entry from teamstate's index, then closes it under its own lock (never both). A teamstate write already holding the entry finishes (its persist may fail on the missing team and reconcile); a later one finds no team (`404 not_in_team`).
- **The start hook runs under the team's lock.** `OnStart` is called holding `entry.mu`, after apply, so the team is already `IN_GAME` in memory (`ByID` and `ForUser` see it) and its `team` event is queued first. The hook must not call back into a write on the same team (it would deadlock); teamstate's `PublishTeam` only reads teams (`ByID`, which takes `Store.mu` briefly) and takes teamstate's own entry lock. Lock order across the two: teams `entry.mu` → teamstate entry.
- **A sign-in during a team write:** `RefreshProfile` takes the same entry lock, so it applies before or after the write, never in between.
- **A sign-in racing the same user's create or join** can leave the old name in memory. If `insertMember` (or `insertTeam`) read `userprofile` before the sign-in's upsert committed, and the hook runs before that write's apply step adds the user to `userTeam`, `RefreshProfile` finds no team and does nothing. The new name shows after the user's next sign-in or a restart. Postgres is never wrong: it's only memory.

## Failure and recovery
| Case | Result |
|---|---|
| Postgres write fails (timeout, connection lost) | `500`; the team is reloaded from Postgres; if that also fails, memory is unchanged and the error is logged |
| Memory and Postgres disagree (rows edited by hand) | The next write to that team fails (`execOne` or a constraint), reconciles, and members get the corrected team |
| Postgres unreachable | Reads keep working; writes fail |
| Backend restarts | Every team reloads exactly, including join order |
| Client disconnects mid-write | The write still completes (it's detached from the request) |
| A second backend on the same database | Refuses to start (`postgres.AcquireInstanceLock`) |

## Limits and how to change them
| Limit | Where | To change |
|---|---|---|
| Team size **6** | `config.toml` `teams.size` (1–16) | Edit and restart |
| Name **32** characters | `config.toml` `teams.name_max_length`, at most the `team.name` CHECK's 32 | Edit and restart; above 32 needs a new migration and `maxNameLength` in `platform/config` |
| 6-digit codes, 900,000 possible | `randomTeamID`, `teamIDPattern`, the `team.id` CHECK | Change all three (new migration) |

## Not decided yet
- **A sign-in racing the same user's create or join can leave the old name in memory** (see [Concurrency and locking](#concurrency-and-locking)). The team shows the old name or avatar until that user's next sign-in, or a restart. Postgres is right; only memory lags. Not fixed: it needs the sign-in hook and the team write to agree on an order, and the effect is cosmetic.
- **A finished team stays forever.** Its members can't join another team; an admin can delete it, which also deletes its result.
- **Only admins can remove a member, and nobody can rename a team.** The leader can only start; a player who won't ready blocks the start until they leave or an admin removes them.
- **No admin route adds a player to a started team.**
- **Team names aren't moderated.** Any name within the length and character rules is accepted.

## Tests
- `tests/store_test.go`:
  - restart reproduces memory exactly, versions aside, which restart at seq 0 in a new epoch (`TestSurvivesRestart`): a started team of `teams.size`, a lobby whose leader left, and a deleted team, with `teams.size` + 4 users; needs `teams.size` ≥ 3 (`minRestartTeam`, a leader and two joiners in the lobby), else it skips
  - reads work with the database closed (`TestReadsDoNotTouchDatabase`)
  - a failed write leaves memory unchanged, and a hand-edited database is reconciled (`TestFailedWriteReconciles`), published as `team: resync` one version on, with `no_team: resync` to the member Postgres dropped
  - `TestVersionAndTypes`: create, join, ready, profile and start each publish their type at the next `version.seq` (1–5); a repeated ready and an unchanged profile publish nothing and keep it
  - `TestRefreshProfile`: a new name and avatar show in `ByID` and publish `team`; the same values again, and a user in no team, publish nothing
  - `TestStartCallsOnStart`: `Rules.OnStart` is called exactly once, with the started team, when it is already `IN_GAME` in `ByID` and its `IN_GAME` `team` event is already on a member's stream; a start before everyone is ready and a second start (`team_locked`) don't call it. Needs `teams.size` ≥ 2 (`minStartTeam`), else it skips
  - `BenchmarkForUser`: parallel `ForUser` reads of one full team; results summed into the `sinkMembers` sink
- `tests/finish_delete_test.go`:
  - `TestFinish`: a lobby team is left alone; a started one becomes `FINISHED` (`team` of type `finished`, stored: a restart keeps it); a repeat changes nothing; an unknown team is `team_not_found`; a finished team's member can't create a team (`already_in_team`)
  - `TestDelete`: every member gets `no_team` (`team_deleted`) and is teamless; the team is gone from memory and Postgres (a restart doesn't bring it back); `OnDelete` is called once with its ID; a member creates a team at once; deleting again is `team_not_found`
- `internal/server/tests/teams_test.go`: every rule and error code in order; leadership handover and deletion; name validation; both races. Team size and name length come from `config.toml`, not repeated numbers.
- `internal/server/tests/auth_test.go` `TestSignInRefreshesTeamProfile`: signing in again with a new name and no picture shows the new name and the old avatar in `GET /api/teams/me`.
- `internal/server/tests/admin_test.go`:
  - `TestAdminGuard`: no token `401`; a player `403 not_admin`; promoted in the database (token still says `PLAYER`) is let in; demoted is refused on the next request
  - `TestAdminRemoveFromLobby`: every code in the table above in order; the removed player gets `no_team` and a member gets the smaller `team`; removing the leader with a new leader; the list route; `last_member`; the removed player creates a team
  - `TestAdminRemoveFromStartedTeam`: the removed player's stream gets `no_team` and no further `team_state`; their scan, selection and state read get `not_in_team`; the rest complete a quadrilateral with the removed player's earlier scan; the removed player joins a lobby team; a restart keeps the smaller roster
  - `TestAdminRemoveRacesScan`: a member's scan racing their removal, for every non-leader: the removal always succeeds, and the scan is accepted (stays in the stack) or refused with `not_in_team`, never accepted after
  - `TestAdminDeleteTeam`: the guard and a bad ID; a member's stream gets `no_team` (`team_deleted`); their state is `not_in_team`; the team leaves the admin's list; a member creates a team at once; deleting again is `team_not_found`
  - `TestLeaderboardAndFinish`: a team through every boss becomes `FINISHED` (`team` of type `finished`) and its selection is `not_in_game`
- `internal/server/tests/events_test.go`: `team`/`no_team` events, stream auth and cleanup; `TestStartPublishesTeamState`: on a start every open stream gets `team` then `team_state`.
