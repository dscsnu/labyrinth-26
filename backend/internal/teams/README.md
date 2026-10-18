# teams

Teams of exactly **6** (`teams.size`) players with a leader, from forming in the lobby to starting the game.

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
| **Leader** | The creator. Starts the game. Doesn't ready up. If the leader leaves the lobby, the longest-standing member takes over. |
| **Status** | `LOBBY` (forming) → `IN_GAME` (started; roster frozen) → `FINISHED` (nothing sets it yet). |
| **Ready** | A non-leader member's "I'm set" flag. The leader can start once all **5** others (`teams.size` − 1) are ready and the team has exactly **6** (`teams.size`). |
| **Join order** | Members are kept in the order they joined. It decides who becomes leader next. |

## The flow
1. **Create.** A signed-in player with no team sends `POST /api/teams {"name"}`. They become the leader and only member of a new `LOBBY` team with a random 6-digit code.
2. **Share the code.** The leader tells teammates the code (out of band: in person, chat).
3. **Join.** Each teammate sends `POST /api/teams/join {"team_id"}` until the team has **6** (`teams.size`). Members are kept in join order.
4. **Ready.** Each non-leader sends `PUT /api/teams/me/ready {"ready": true}`. They can un-ready, leave and rejoin while in the lobby.
5. **Start.** The leader sends `POST /api/teams/me/start`. With exactly **6** (`teams.size`) members, all non-leaders ready, the team becomes `IN_GAME` and its roster is frozen. From here `teamstate` takes over.
6. **Live updates.** Every member's event stream (`GET /api/teams/me/events`) gets the whole team after every change, and `no_team` when they leave.

- **Why the leader doesn't ready up.** Starting is the leader's "ready". A separate flag would only add a state that can disagree with pressing start.
- **Why join order decides the next leader.** It's stable, visible to everyone, and survives a restart (`joined_at`, ties broken by `user_id`).
- **Why the roster freezes at start.** `teamstate` reads the team without its lock and relies on the members never changing once `IN_GAME`.

## How each file works

### `api.go`: routes
- **`Routes`** registers six routes behind the auth middleware. The live stream is registered by `internal/server`, because it carries events from several use cases.
- Each handler decodes and checks the body, calls one `Store` method with the caller's ID (`identity.UserID`), and writes the result in the envelope.
- **`create`** trims the name (`strings.TrimSpace`); then **`validName`** enforces 1 to **32** (`teams.name_max_length`) characters (counted as runes) with no control characters. **`teamIDPattern`** requires exactly 6 digits.
- **`fail`** maps rule errors to a status and code via `errorCodes`; anything else is logged and becomes `500`.

### `read.go`: types and reads
- **Types:** `Team`, `Member`, `Status` (`Lobby`, `InGame`, `Finished`), and the error `ErrNotInTeam`.
- **Reads**, from memory only:
  - `ForUser(userID)`: two map lookups under a brief shared lock, then a lock-free snapshot load. About 60 ns per read, measured with `BenchmarkForUser` (one team of **6** (`teams.size`) members, 16 goroutines reading at once, on an i7-12650H)
  - `ByID(teamID)`
  - `All()`: every team, sorted by ID. Copies the index under the shared lock, then loads each snapshot lock-free. For admin views; the restart test compares `All()` (plus each member's `ForUser`) before and after.
  - `Snapshot(userID)`: the `team`/`no_team` event for a new stream
  - `Store.Size()`: the method returning **6** (`teams.size`), the team size from `Rules`. Used by tests.

### `write.go`: writes, each written through
- **`Create`**, **`Join`**, **`Leave`**, **`SetReady`** and **`Start`** each follow the [write-through](#the-write-through) steps, numbered in the code.
- **`RefreshProfile(userID, name, avatarURL)`** updates a member's `Name` and `AvatarURL` in memory and publishes `team`. Called by auth's sign-in hook (wired in `server.Build`) after every successful sign-in, with the user as `userprofile` now stores it. Steps: 1 lock (`lockUsersTeam`), 2 validate (anything changed?), 3 build (clone `Members`, set the two fields), 5 apply, 6 publish. **No persist:** the user use case has already saved `userprofile`, and teams reads names and avatars from it at load. A no-op when the user is in no team or nothing changed. Returns nothing.
- **`equalPtr(a, b)`**: both `nil`, or both pointing to equal strings. Compares avatars.
- The rule errors are declared here.

### `db.go`: SQL
- **`selectAll`** reads every team and every member, in join order. `selectOne` / `selectTeam` read one.
- **`insertTeam`**: one transaction that inserts `team` (`ON CONFLICT (id) DO NOTHING`, giving `errTeamIDTaken` on a collision), inserts the leader into `teammember`, and reads the team back.
- **`insertMember`**: inserts and returns the member joined with their profile, in one statement (`WITH ins AS (…)`).
- **`removeMember`**: one transaction that deletes the member, moves the leader if needed, and deletes the team if it's now empty.
- **`updateReady`** and **`updateStarted`**. `updateStarted` returns Postgres's `now()` and only matches a `LOBBY` team.
- **Helpers:**
  - `execOne` fails unless exactly one row changed, which is how a memory/database disagreement is detected.
  - `mapMemberInsertErr` maps the `teammember_user_id_key` violation to `errUserInTeam`.

### `store.go`: the in-memory layout
- **`Load`** builds memory at startup.
- **`persist`** runs a write detached from the request, capped at **5 s** (`storage.write_timeout`).
- **`lockUsersTeam`** finds and locks a user's team, retrying if the user moved team between lookup and lock.
- **`reconcile`** reloads a team after a failed write.
- **`publish`** and **`teamEvent`** send events. **`randomTeamID`** picks codes from 100000–999999.

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
1. **lock** the team's entry (`lockUsersTeam`, or the entry by code for `Join`)
2. **validate** the rules against the snapshot
3. **build** the next snapshot
4. **persist** via `db.go`, detached from the request, capped at **5 s** (`storage.write_timeout`)
5. **apply**: swap the snapshot, update `teams`/`userTeam`
6. **publish** `team` to members (`no_team` to a leaver)

| Function | Steps, in code order | `db.go` call | Why the order differs (AGENTS.md §6) |
|---|---|---|---|
| `Create` | 2, 4, 3, 5, 6 (no 1) | `insertTeam` | (c) a new team has no entry to lock until apply makes it; the unique constraint on `teammember.user_id` guards a concurrent create or join. (b) Postgres sets `created_at` and `joined_at`, so the snapshot is the team it returns |
| `Join` | 1, 2, 4, 3, 5, 6 | `insertMember` | (b) Postgres sets `joined_at` and returns the member with their profile |
| `Leave` | 1–6 | `removeMember` | |
| `SetReady` | 1–6 | `updateReady` | |
| `Start` | 1, 2, 4, 3, 5, 6 | `updateStarted` | (b) Postgres sets `started_at` |
| `RefreshProfile` | 1, 2, 3, 5, 6 (no 4) | none | memory-only: `user` already saved `userprofile` |

**On a failed persist:** `reconcile` reloads the team from Postgres, re-indexes its members, publishes, and the caller gets `500`. **SetReady** to the current value returns before step 4. **RefreshProfile** skips step 4 entirely (the data is already in `userprofile`), so it can't fail and never reconciles.

## API
All routes need `Authorization: Bearer <token>` (`401` without).

| Method | Path | Body | Success |
|---|---|---|---|
| GET | `/api/teams/me` | none | `200` team |
| POST | `/api/teams` | `{"name": "Seven Evil Exes"}` | `201` team |
| POST | `/api/teams/join` | `{"team_id": "482913"}` | `200` team |
| POST | `/api/teams/leave` | none | `200`, no payload |
| PUT | `/api/teams/me/ready` | `{"ready": true}` | `200` team |
| POST | `/api/teams/me/start` | none | `200` team |

**Team JSON** (payload, and the `team` event's data):
```json
{"id": "482913", "name": "Seven Evil Exes",
 "leader_id": "6f1c…", "status": "LOBBY",
 "created_at": "2026-10-17T12:00:00Z", "started_at": null,
 "members": [
   {"id": "6f1c…", "name": "Jia Khot", "avatar_url": "https://…", "is_ready": false, "joined_at": "2026-10-17T12:00:00Z"},
   {"id": "9a2b…", "name": "Aarav", "avatar_url": null, "is_ready": true, "joined_at": "2026-10-17T12:00:04Z"}
 ]}
```
Emails are never included. **Error body:** `{"success": false, "code": "team_full", "message": "team is full"}`.

**Live events** on `GET /api/teams/me/events`: `team` (the JSON above), or `no_team` (`{}`). `team` is also sent when a member signs in again with a changed name or avatar.

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
| Not exactly **6** (`teams.size`) members | `409 team_not_full` |
| Any non-leader not ready | `409 not_all_ready` |
| Success | `IN_GAME`, `started_at` = Postgres `now()`; roster frozen; `team` to all members |

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
- **The leader starts while a member leaves:** exactly one wins, and the team stays consistent (`TestConcurrentStartAndLeave`).
- **A user creates and joins at once:** the unique constraint on `teammember.user_id` lets only one through.
- **`lockUsersTeam`** re-checks membership after taking the lock, and retries up to 3 times if the user moved team.
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
- **`FINISHED` is never set.** Nothing ends a game yet; when it does, decide whether a finished team's members may form new teams.
- **No way to kick a member or rename a team.** The leader can only start; a player who won't ready blocks the start until they leave.
- **Team names aren't moderated.** Any name within the length and character rules is accepted.

## Tests
- `tests/store_test.go`:
  - restart reproduces memory exactly (`TestSurvivesRestart`): a started team of `teams.size`, a lobby whose leader left, and a deleted team, with `teams.size` + 4 users; needs `teams.size` ≥ 3 (`minRestartTeam`, a leader and two joiners in the lobby), else it skips
  - reads work with the database closed (`TestReadsDoNotTouchDatabase`)
  - a failed write leaves memory unchanged, and a hand-edited database is reconciled (`TestFailedWriteReconciles`)
  - `TestRefreshProfile`: a new name and avatar show in `ByID` and publish `team`; the same values again, and a user in no team, publish nothing
  - `BenchmarkForUser`: parallel `ForUser` reads of one full team; results summed into the `sinkMembers` sink
- `internal/server/tests/teams_test.go`: every rule and error code in order; leadership handover and deletion; name validation; both races. Team size and name length come from `config.toml`, not repeated numbers.
- `internal/server/tests/auth_test.go` `TestSignInRefreshesTeamProfile`: signing in again with a new name and no picture shows the new name and the old avatar in `GET /api/teams/me`.
- `internal/server/tests/events_test.go`: `team`/`no_team` events, stream auth and cleanup.
