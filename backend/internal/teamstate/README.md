# teamstate

What each team does in the game: the shape it has selected, its stack of scans toward a pattern of that shape, the waypoints it has visited, the patterns and shapes it has completed, and its cast log. There is **no time limit**: nothing changes because time passed, only because a member acted.

Every number below that comes from `config.toml` is shown with its current value and its key, e.g. **6** (`game.max_pattern_points`). Change the file, not the code.

## Contents
1. [Concepts](#concepts)
2. [The game flow](#the-game-flow)
3. [How each file works](#how-each-file-works)
4. [Data model: memory](#data-model-memory)
5. [Data model: Postgres](#data-model-postgres)
6. [Memory ↔ Postgres](#memory--postgres)
7. [Wrong scans and poisoned attempts](#wrong-scans-and-poisoned-attempts)
8. [Walkthroughs](#walkthroughs)
9. [The write-through](#the-write-through)
10. [API](#api)
11. [Rules and cases](#rules-and-cases)
12. [Concurrency and locking](#concurrency-and-locking)
13. [Failure and recovery](#failure-and-recovery)
14. [Limits and how to change them](#limits-and-how-to-change-them)
15. [Not decided yet](#not-decided-yet)
16. [Tests](#tests)

The waypoints, shapes and patterns themselves are static content owned by the **atlas** use case: how they're loaded, validated and stored (indexes, bitmasks, `FitsShape`) is in [`internal/atlas/README.md`](../atlas/README.md). This README covers what teams do with them.

## Concepts
| Term | Meaning |
|---|---|
| **Atlas** | The static content (waypoints, shapes, patterns), loaded once at startup by the atlas use case and read here through the [`Atlas` interface](#storego-the-in-memory-layout). Never changes while the backend runs. |
| **Waypoint** | A physical location with a QR code. Identified by a **4-digit ID** (`"1001"`; leading zeros allowed). |
| **Pattern** | A set of **1 to 6** (`game.max_pattern_points`) waypoints that forms a shape on the map. Identified **by that set**, e.g. `["1001","1002","1007","1008"]`, in any order. Its database `id` is only an internal key. |
| **Shape** | What a pattern looks like: `square`, `triangle`, `collinear`, `tree`, `lightning` in the 2025 seed. A shape has many patterns. Completing any one completes the shape. |
| **Selection** | The shape a team is currently attempting. At most one per team. |
| **Attempt** | Everything between a selection and the next selection, clear or cast. Its scans are the stack. |
| **Stack** | The attempt's accepted scans, oldest at the **bottom**, newest on **top**. It holds **one scan per waypoint**; any member may scan any number of them. Its correct scans always fit inside one pattern of the selected shape, so there are at most **6** (`game.max_pattern_points`) of them, plus at most **one wrong scan**, always on top. Capacity: **7** (`game.max_pattern_points` + 1). Scans never leave on their own: only a selection, clear or cast empties it. |
| **Wrong scan** | A scan whose waypoint, together with the scans already in the stack, isn't part of any pattern of the selected shape (`!FitsShape`). It's **pushed** (not rejected), marked `wrong`, and the player is **told**: outcome `wrong_scan`, and `stack[].wrong: true` on every phone. |
| **Poisoned** | The stack holds a wrong scan (`State.Poisoned()`, `poisoned: true` in JSON). The attempt can't cast, and every further scan is refused with `409 selection_poisoned` until someone selects again (which purges the stack). See [Wrong scans and poisoned attempts](#wrong-scans-and-poisoned-attempts). |
| **Cast** | A completion: the moment the stack becomes exactly a pattern of the selected shape. The client calls it casting a spell. Every cast is logged, repeats included: in memory (`State.Casts`) and in `team_cast`. `team_pattern` keeps only the first completion of each pattern. |
| **Cast log** | `State.Casts`: every cast the team has made, oldest first, loaded from `team_cast` at startup. `GET /api/teams/me/casts` returns it newest first. |
| **Last cast** | `State.LastCast()`, `last_cast` in JSON: the cast log's newest entry (pattern, shape, who, when, whether the pattern was already completed, damage). Kept until the next cast; a new selection or a clear doesn't remove it. |
| **Spell lifecycle** | What the client shows as a spell, read from `team_state` alone, with no event of its own: it **starts** when `selection` is set, members **add their points** as `stack` grows (`stack[].user_id` says who scanned each), it **fails** when `poisoned` turns true, and it **finishes** when `last_cast` changes (a new `cast_at`). |
| **Visited waypoints** | Every waypoint the team has ever had a scan accepted at, under any selection, wrong scans included. |
| **Clock** | Where the store gets the time it stamps selections, scans and casts with: one method, `Now()`. `systemClock` is the real one; tests pass a fake through `Rules.Clock`. Time only orders and labels events; no rule depends on it. |
| **Game rules** | What `GET /api/game/shapes` returns: the selectable shapes (the client's "types") with their pattern counts, sizes and damage ranges, plus `max_pattern_points`. Static while the backend runs. No timing rules: there's no time limit. |

**Client words.** The client's **"type"** is our **Shape**, its **"shape"** is our **Pattern**, its **"point"** is our **Waypoint**, and its **"buffer"** is our **Stack**. This README and the code always use the code's words. The full vocabulary, spells included, is in [`internal/atlas/README.md`](../atlas/README.md).

## The game flow
0. **Start.** When the leader starts the team (teams' `Start`; any size from 1 to **6** (`teams.size`) may start, once every non-leader is ready), every member's stream gets the `IN_GAME` `team` and then the team's `team_state`, empty: no selection, no stack, no progress, `poisoned: false`. teams calls `PublishTeam` through its `Rules.OnStart` hook, wired in `server.Build`, because teams may not import teamstate (AGENTS.md §4). The play screen has its state at once, without reconnecting.
1. **Select.** Any member selects a shape: `PUT /api/teams/me/state/selection {"shape":"square"}`. **Every selection purges the stack**, reselecting the same shape included. Reselecting is how a team restarts an attempt.
2. **Scan.** Members scan waypoints: `POST /api/teams/me/state/scans {"waypoint_id":"1001"}`. No time limit, and any member may scan any number of the pattern's waypoints (one player can scan a whole pattern). A scan is **rejected with an error and not pushed** if:
   - no shape is selected: `409 no_shape_selected`
   - the attempt is **poisoned**: `409 selection_poisoned`
   - its **waypoint is already in the stack**, whoever scanned it, the same member included: `409 waypoint_already_scanned`

   Otherwise it's pushed on top. If the waypoint, with the stack, can't be part of any pattern of the selected shape, it's pushed **marked wrong**, the response says `wrong_scan`, and the attempt is poisoned.
3. **Complete (cast).** The moment the stack is **exactly** one of the selected shape's patterns, the pattern and its shape are marked complete, the cast is appended to the cast log (`State.Casts`, `team_cast`) and becomes `last_cast`, and **the stack and the selection are purged**. The team selects again for its next attempt.
4. **Restart.** After a wrong scan, any member reselects (the same shape or another): the stack, wrong scan included, is purged and the team scans again.
5. **Give up.** `DELETE /api/teams/me/state/selection` clears the selection and the stack.

**"Part of a pattern" narrows as the team scans.** The team selects a shape, not a pattern, so the first scan is correct if it's in *any* pattern of the shape. Each later scan must keep the stack inside at least one pattern of the shape that contains everything already scanned. A scan that doesn't is wrong.

**Why the shape is selected first.** With the 2025 data any three waypoints form a triangle or a collinear pattern, so a team scanning a square passes through a "triangle" after its third scan. Only the selected shape can complete, which is what makes 4-waypoint shapes possible.

**Why a wrong scan is pushed, not rejected.** The scan really happened: someone walked to that waypoint. Recording it (in the stack, in `team_scan`, in visited `waypoints`) keeps the audit trail honest, and showing it marked on every phone tells the whole team *which* point broke the attempt and who scanned it.

**Why a wrong scan poisons instead of being dropped.** If the server silently dropped the wrong scan, a team could probe which waypoints belong to the selected shape for free, one scan at a time, with no cost. Poisoning makes a mistake cost a restart: every correct scan of the attempt is lost too.

**Why no time limit.** The game used to give a pattern's scans 1 minute (plus grace) bottom to top. That's gone: an attempt lasts until it casts, is restarted or is cleared. So there are no timers, no stale scans, and no client countdown.

**Retries.** A phone resending a scan gets `409 waypoint_already_scanned` (or `selection_poisoned`, if its first copy was wrong); the app should treat that as "already in" and refresh from the returned state or the live event.

## How each file works

### `api.go`: routes
- **`Routes`** registers six routes behind the auth middleware (see [API](#api)).
- **Handlers:**
  - `shapes` returns `Store.Game()` to any signed-in user, in a team or not.
  - `getMine` returns the caller's team state (`Store.ForUser`).
  - `casts` returns the caller's team's cast log, newest first (`Store.CastsForUser`).
  - `selectShape` decodes `{"shape"}` and calls `Store.Select`.
  - `clearSelection` calls `Store.ClearSelection`.
  - `scan` decodes `{"waypoint_id"}` and calls `Store.Scan`, returning a `ScanResult`.
- **`fail`** maps errors to statuses and codes via the `errorCodes` table; anything unmapped is logged and becomes `500`.
- **⚠** The scan body is the raw waypoint ID, so it doesn't prove presence. See [Not decided yet](#not-decided-yet).

### `read.go`: types and reads
- **Types:**
  - `State`: the in-memory per-team snapshot, made of `Selection`, `Stack`, `StackScan` and `[]Cast` (see [Data model: memory](#data-model-memory)).
  - `State.Poisoned()`: true if any scan in the stack is `Wrong`.
  - `State.LastCast()`: the last element of `Casts`; `ok` is false if the log is empty.
  - `Cast`: one completed pattern (`Pattern`, `By`, `At`, `AlreadyCompleted`).
  - `View`, `SelectionView`, `CastView`, `StackScanView`: the JSON shapes. `CastView` is `last_cast` and each entry of the cast log: `pattern` (sorted waypoint IDs), `shape`, `cast_by`, `cast_at`, `already_completed`, `damage`. `StackScanView` carries `wrong`; `View` carries `poisoned`.
  - `Game`, `GameShape`, `DamageRange`: the JSON of `GET /api/game/shapes`, including each shape's `damage` range (`{"min": …, "max": …}`).
- **`Stack` methods** (all on values; none modifies the receiver):
  - `NewStack(maxPoints)` is an empty stack with `Cap` = `maxPoints` + 1 (exported so tests can build one).
  - `Len()` counts scans.
  - `At(i)` is the i-th from the bottom (`At(0)` the oldest).
  - `Push(sc)` returns the stack with `sc` on top. It **panics** on a full stack: `Scan` never pushes onto one (correct scans fit one pattern, and nothing is pushed after a wrong scan), so a full push would be a bug.
- **`newGame(atlas, rules)`** builds the `Game` once, in `Load`: for each shape in index order (`ShapeCount`, `Shape(i)`), its ID and name, how many patterns it has (`len(ShapePatterns(i))`), the distinct sizes of those patterns (`Pattern(p).Waypoints.Len()`), ascending, and the minimum and maximum `Pattern(p).Spell.Damage`; plus `MaxPatternPoints` from `Rules`.
- **Reads**, all from memory:
  - `Game()`: the prebuilt `Game`; no lock, it never changes
  - `ForTeam(teamID)` → `State` (the empty state for a team with no entry)
  - `ForUser(userID)` → `View`, or `ErrNotInTeam`
  - `CastsForUser(userID)` → `[]CastView`, newest first, or `ErrNotInTeam`; `[]` before the first cast
  - `Snapshot(userID)`: the live event for a new stream
- **`view`** converts indexes and bits to IDs through the atlas (`WaypointIDs`, `Pattern`, `Shape`, `Waypoint`), times to UTC, builds `last_cast` from `State.LastCast()` (`null` when the log is empty), lists every stack scan with its `wrong` flag, and sets `poisoned` from `State.Poisoned()`.
- **`castView`** turns one `Cast` into a `CastView`, the shape being the pattern's `Shape` and `damage` the pattern's precomputed `Spell.Damage` (from `pattern.damage`, loaded by the atlas). Used by `view` and `CastsForUser`.

### `write.go`: writes, each written through
- **`Select`**, **`ClearSelection`** and **`Scan`** each follow the [write-through](#the-write-through) steps, numbered in the code.
- **Lock-free checks first** (§6 variation (a)): `Select` resolves the shape with `Atlas.ShapeIndex`, `Scan` the waypoint with `Atlas.WaypointIndex`, both before the team and the lock.
- **`Scan`'s checks**, in order, under the lock: no selection → `ErrNoShapeSelected`; `cur.Poisoned()` → `ErrSelectionPoisoned`; the waypoint already in the stack → `ErrWaypointAlreadyScanned`. Then `Atlas.FitsShape(shape, stack ∪ {w})` decides whether the scan is **wrong** (pushed, marked `Wrong`, outcome `wrong_scan`). Completion is `!wrong` and `Atlas.PatternFor(stack ∪ {w})` (one map lookup) being a pattern whose `Atlas.Pattern(p).Shape` is the selected shape. On completion, `Atlas.WaypointIDs` gives `ScanResult.Pattern`, `Atlas.Pattern(p).ID` is the key written to `team_pattern` and `team_cast`, `AlreadyCompleted` is `cur.Patterns.Has(p)`, and the new `Cast{Pattern: p, By: caller, At: now, AlreadyCompleted}` is appended to the log **copy-on-write**: `append(slices.Clip(cur.Casts), cast)`. `Clip` sets the slice's capacity to its length, so `append` always allocates a new backing array and the current snapshot's slice is never written to.
- **`inGameTeam`** fetches the caller's team and requires `IN_GAME` (lock-free: a started team's status never goes back).
- **`isMember(teamID, userID)`** reads the team's current roster through `Teams.ByID` (lock-free) and reports whether the user is in it. `Select`, `ClearSelection` and `Scan` call it **again under the entry lock**, right after `1. lock`, and refuse with `ErrNotInTeam` (`404 not_in_team`) if an admin removed the caller since `inGameTeam` looked them up. A started team's roster can shrink (teams' `RemoveMember`), so the lock-free lookup alone isn't enough.
- **`RosterChanged(teamID)`** is teams' `Rules.OnRosterChange` (wired in `server.Build`): an admin removed a member from a started team. It locks the entry (`lockTeam`) and drops `entry.members`, the roster cache, so the next publish re-reads the roster from teams and no longer reaches the removed player. **Memory-only** (AGENTS.md §6): the roster is teams' data, already saved. **Publishes nothing:** the team's `State` didn't change (the removed player's scans and casts stay), and teams has already sent the rest `team` and the removed player `no_team`.
- **`PublishTeam(teamID)`** sends the team's current state to every member as `team_state`, changing nothing. teams calls it through `teams.Rules.OnStart` (wired in `server.Build`) right after a team starts, so members get the new team's empty state after its `team` event. Steps: `2. validate, before 1. lock` (`Teams.ByID`: the team exists and is `IN_GAME`, else nothing is published), `1. lock` (`lockTeam`, which creates the empty entry if the team had none), `6. publish` (`publish`, which fills the entry's roster cache on this first publish of the started team). Not a write: no build, persist or apply. It takes the entry lock so its event is ordered with the team's writes.
- **Declared here:** the errors (`ErrNotInGame`, `ErrUnknownWaypoint`, `ErrUnknownShape`, `ErrNoShapeSelected`, `ErrWaypointAlreadyScanned`, `ErrSelectionPoisoned`), the `Outcome…` constants (`scan_added`, `wrong_scan`, `pattern_completed`), and `ScanResult`.

### `db.go`: SQL
Only the per-team tables. The content tables are read by the atlas (`internal/atlas/db.go`).
- **`selectTeams(ctx, teamID, stackSize)`** reads every team's stored state, or one team's (`teamID != ""`), into `teamRows`: distinct visited waypoints, completed patterns, **every** cast (`casts`, `castRow`s, `ORDER BY team_id, id`: oldest first), selection, and the stack. Used by `Load` and `reconcile`, with `stackSize` = `game.max_pattern_points` + 1. The stack query is a `JOIN LATERAL`: for each selection, the team's newest `stackSize` `team_scan` rows with `id > after_scan_id`, returned oldest first, with the `wrong` flag.
- **`insertScan`**: one `team_scan` row, with the `wrong` flag (a scan that doesn't complete anything: a correct one, or a wrong one).
- **`insertCompletingScan`**: one `pgx.BeginFunc` transaction, four statements in order:
  1. `INSERT team_scan`: the scan itself (`wrong` is always false here)
  2. `INSERT team_pattern … ON CONFLICT DO NOTHING`: the first completion only
  3. `INSERT team_cast (team_id, pattern_id, cast_by, cast_at, already_completed)`: every cast, repeats included
  4. `DELETE team_selection`: which also empties the stack
- **`upsertSelection`**: upserts `team_selection` with `after_scan_id` = the team's latest `team_scan.id` (0 if none). That's the purge: earlier scans (a wrong one included) are no longer after it.
- **`deleteSelection`**: deletes `team_selection`, and fails unless exactly one row went (memory had a selection, so Postgres must too).

### `store.go`: the in-memory layout
- **`Teams`** is what team state needs from the teams use case: `ForUser(userID)` and `ByID(teamID)`.
- **`Atlas`** is what team state needs from the atlas use case, declared here (AGENTS.md §4) and satisfied by `*atlas.Atlas`. The atlas never changes after startup, so every call is lock-free:

  | Method | Teamstate uses it for |
  |---|---|
  | `WaypointIndex(id)` | `Scan`: unknown waypoint → `404 unknown_waypoint`; `stateOf`: a stored stack scan's waypoint |
  | `ShapeIndex(id)` | `Select`: unknown shape → `404 unknown_shape`; `stateOf`: a stored selection's shape |
  | `PatternByID(id)` | `stateOf`: a stored `team_pattern` key, or a `team_cast.pattern_id`, → pattern index |
  | `SetOf(ids)` | `stateOf`: stored visited waypoints → `State.Waypoints` |
  | `PatternFor(set)` | `Scan`: is the stack plus this scan exactly a pattern? |
  | `FitsShape(shape, set)` | `Scan`: is the scan **wrong**? |
  | `WaypointIDs(set)` | `view` (visited waypoints, completed patterns), `castView` (`pattern`), `ScanResult.Pattern` |
  | `ShapePatterns(shape)` | `newGame`: each shape's pattern count, sizes and damage range |
  | `ShapeCount()` | `newGame`: how many shapes |
  | `Waypoint(i)` | `view`: a stack scan's waypoint ID |
  | `Shape(i)` | `view` (completed shapes, selection), `castView` (`shape`), `newGame` |
  | `Pattern(i)` | `Scan` (the completed pattern's shape and Postgres `ID`), `stateOf` (a completed pattern's shape), `view` (patterns), `castView` (waypoints, shape, `Spell.Damage`), `newGame` |
- **`Clock`** is the store's time source, an interface with one method, `Now()` (UTC, truncated to microseconds). **`systemClock`** is the real one: `time.Now().UTC().Truncate(time.Microsecond)`.
- **`Rules`** are the settings from `config.toml`: `MaxPatternPoints` (`game.max_pattern_points`; the stack holds that many plus one wrong scan), `WriteTimeout` (`storage.write_timeout`), and `Clock` (nil means `systemClock`; tests pass a fake; not in config.toml).
- **`Load(ctx, pg, atlas, teams, hub, log, rules)`** reads every team's stored state (`selectTeams` with `stackSize` = `MaxPatternPoints` + 1), checking it against the atlas, and builds the `Game` (`newGame`). It runs after `atlas.Load` (it's handed the `*atlas.Atlas`) and after `teams.Load` (it reads teams through the `Teams` interface). No timers, no goroutines.
- **`emptyState`** is a `State` with an empty stack of capacity `MaxPatternPoints` + 1.
- **`stateOf(teamRows)`** rebuilds a `State` from IDs through the atlas, failing on unknown waypoints, patterns or shapes. Every `team_cast` row becomes a `Cast` in `State.Casts`, oldest first (`cast: unknown pattern N` if the atlas lacks it). Stack rows are pushed oldest first, with their `wrong` flag.
- **`lockTeam`** gets or creates a team's entry and locks it.
- **`persist`** runs a `db.go` write detached from the request, capped at **5 s** (`storage.write_timeout`).
- **`reconcile`** reloads one team's state from Postgres after a failed write and publishes it. If the reload fails too, memory stays as it was.
- **`publish(e, teamID, st)`**, called with `e.mu` held, sends `team_state` to every member. The recipients are **`e.members`**, the team's roster cached on the first publish that finds the team `IN_GAME` (its roster then only changes by an admin's removal, and `RosterChanged` drops the cache when it does). Until then, and after a drop, it reads the roster through `Teams.ByID`. So a publish reads nothing outside the entry: no index lock, no lookup.
- **`event`** encodes the `View` once as a `live.NewEvent("team_state", …)`; every member's stream gets the same bytes.
- **`now`** returns `clock.Now()`: UTC, truncated to microseconds (Postgres's precision), so a restart reproduces times exactly.

## Data model: memory
```
Store
  rules    Rules                     from config.toml
  db       db                        the pool, for db.go
  atlas    Atlas                     the static content (*atlas.Atlas); read with no lock
  game     Game                      built once by newGame; read with no lock
  teams    Teams                     the teams use case (*teams.Store)
  hub      *live.Hub                 where team_state events go
  log      *slog.Logger
  mu       sync.RWMutex              guards the map below; never held during I/O
  states   map[teamID]*entry         created on a team's first write (or PublishTeam)
  clock    Clock                     Rules.Clock, or systemClock

entry
  mu          sync.Mutex             writers to this team queue here
  cur         atomic.Pointer[State]  the current snapshot; readers load it lock-free
  members     []uuid.UUID            the user IDs events go to: the roster, cached
                                     by publish once the team is IN_GAME, dropped
                                     by RosterChanged on an admin's removal; nil
                                     until the next publish refills it. Guarded by mu

State                                (never modified once stored)
  Waypoints  atlas.WaypointSet  uint64      visited waypoints, bit i = waypoint index i
  Patterns   atlas.PatternSet   [32]uint64  completed patterns, bit i = pattern index i
  Shapes     atlas.ShapeSet     uint64      completed shapes, bit i = shape index i
  Selection  Selection
    Active   bool                    false → no selection; other fields zero
    Shape    int                     the shape's index in the atlas
    By       uuid.UUID               the member who selected
    At       time.Time               when
  Stack      Stack                   the attempt's scans
  Casts      []Cast                  the cast log, oldest first; copy-on-write
    Pattern          int             the pattern's index in the atlas
    By               uuid.UUID       the member whose scan completed it
    At               time.Time       when
    AlreadyCompleted bool            the team had cast this pattern before

Stack                                a value; Push returns a new one
  scans    [16]StackScan             16 = config.MaxPatternPoints (15) + 1;
                                     only the first Cap slots are used
  n        int                       0…Cap scans held
  Cap      int                       game.max_pattern_points + 1 (7)

StackScan
  Waypoint int                       the waypoint's index in the atlas
  UserID   uuid.UUID                 who scanned
  At       time.Time                 when
  Wrong    bool                      didn't fit the selected shape; poisons the
                                     attempt; exposed as stack[].wrong
```
- **Size:** a `State` is a fixed **1264 bytes** (`Stack` 912, `Selection` 56, the `Casts` slice header 24; measured with `unsafe.Sizeof` on a copy of the layout, linux/amd64), so a new snapshot is a plain struct copy. The cast log's backing array is on the heap, **56 bytes per cast**. 10,000 teams' states are about 13 MB, plus 0.5 MB for every 1 cast per team on average (20 casts each: about 11 MB).
- **Why the cast log is a slice and copy-on-write.** Its length isn't bounded, so it can't be a fixed array. Snapshots share the backing array until a cast: `Scan` builds the new log with `append(slices.Clip(cur.Casts), cast)`, which always copies, so a reader holding the old snapshot keeps seeing the old log. A cast costs one copy of the log (56 bytes per earlier cast), and only casts pay it: selections and scans copy the slice header only.
- **`LastCast()` is derived**, not stored: the log's last element. So `last_cast` and the log can never disagree.
- **The sets are the atlas's types** (`atlas.WaypointSet`, `atlas.PatternSet`, `atlas.ShapeSet`), over the atlas's indexes. How they're laid out and why: [`internal/atlas/README.md`](../atlas/README.md), "How waypoints and patterns are stored".
- **Why a plain array, not a circular buffer.** Scans only ever leave the stack all at once (a purge builds a new empty stack), so nothing is dropped from the bottom and `At(i)` is just `scans[i]`.
- **The array is 16 long** although only **7** (`game.max_pattern_points` + 1) are used, because Go array sizes must be constants and `game.max_pattern_points` is read at startup. `config.MaxPatternPoints` (15) is the most `game.max_pattern_points` may be, and the extra slot is for the one wrong scan.

## Data model: Postgres
The content tables (`waypoint`, `shape`, `pattern`, `pattern_waypoint`, migration `0003`; `pattern.damage`, `0008`) belong to the atlas: see [`internal/atlas/README.md`](../atlas/README.md), "Data model: Postgres". The per-team tables below refer to them (`team_scan.waypoint_id` → `waypoint`, `team_pattern.pattern_id` → `pattern`, `team_selection.shape_id` → `shape`, `team_cast.pattern_id` → `pattern`).

Per team, written through:

| Table | Migration | Columns | Constraints and what they back |
|---|---|---|---|
| `team_scan` | `0003`, `0006` | `id` BIGINT identity, `team_id`, `waypoint_id`, `user_id`, `scanned_at`, `wrong` | One row per **accepted** scan (rejected scans are never written): the audit trail, and the source of the stack. `wrong` (`0006`) marks a scan that didn't fit the selected shape, so a restart reproduces the poisoned attempt. `0005` records the table's `COMMENT`, correcting 0003's SQL comment ("every scan, including repeats"). One index, `team_scan_team_id_idx` on `(team_id, id DESC)` (`0004`), serves the stack query and every other query by team; `0005` dropped 0003's redundant `team_scan_team_idx` on `(team_id)`. |
| `team_pattern` | `0003` | `team_id`, `pattern_id`, `completed_at` | PK `(team_id, pattern_id)`: a pattern counts once |
| `team_selection` | `0004` | `team_id`, `shape_id`, `selected_by`, `selected_at`, `after_scan_id` BIGINT | PK `team_id`: one selection per team. `after_scan_id` is the team's latest `team_scan.id` when the shape was selected: the stack is the scans after it |
| `team_cast` | `0007` | `id` BIGINT identity PK, `team_id` VARCHAR(6), `pattern_id` INTEGER, `cast_by` UUID, `cast_at` TIMESTAMPTZ, `already_completed` BOOLEAN, all `NOT NULL` | One row per **cast**, repeats included (unlike `team_pattern`): the cast log. `pattern_id` → `pattern` (a cast is of a real pattern); `cast_by` → `userprofile` (a real user); `already_completed` records whether `team_pattern` already had the pattern. `id` orders casts: `Load` reads them `ORDER BY team_id, id`. Index `team_cast_team_id_idx` on `(team_id, id DESC)` serves a one-team read (`reconcile`). No unique constraint: any number of casts per team and pattern |

Every per-team table has `team_id` → `team` with `ON DELETE CASCADE`. `team_cast` is written only inside `insertCompletingScan`'s transaction, so a cast is logged exactly when its scan and its `team_pattern` row are.

**The stack isn't a table.** It's derived: the `team_scan` rows with `id > after_scan_id` (at most **7**, `game.max_pattern_points` + 1, which is the query's `LIMIT`). Selecting moves `after_scan_id` to the latest scan (purge); completing or clearing deletes the selection (no selection, no stack). Scans never leave an attempt any other way, so the rows after `after_scan_id` are exactly the stack.

## Memory ↔ Postgres
| Postgres | Rebuilt in memory (by `Load`, or `reconcile` for one team) |
|---|---|
| `waypoint`, `shape`, `pattern`, `pattern_waypoint` | Not here: the atlas, rebuilt by `atlas.Load` before this `Load` ([`internal/atlas/README.md`](../atlas/README.md)) |
| `DISTINCT waypoint_id FROM team_scan` | `State.Waypoints` |
| `team_pattern` | `State.Patterns`, and `State.Shapes` from each pattern's shape |
| `team_selection` | `State.Selection` |
| every `team_cast` row, `ORDER BY team_id, id` | `State.Casts`, oldest first; `last_cast` is its last element |
| newest **7** (`game.max_pattern_points` + 1) `team_scan` with `id > after_scan_id`, by `id` | `State.Stack` (pushed oldest first, with the `wrong` flag, so `poisoned` comes back too) |
| (none: built from the atlas and `config.toml`) | `Store.game`, rebuilt by `newGame` at every `Load` |
| (none: memory-only, from teams) | `entry.members`, the roster cache: refilled from `Teams.ByID` on each team's first publish after a restart. teams reloads rosters from `teammember` (an admin's removal deleted the row), and `RosterChanged` drops the cache on every removal, so the cache always matches |

**Nothing else is held only in memory.** There are no timers and nothing changes with time, so a restart reproduces memory exactly: `TestRestartRestoresEverything` and `TestCastLog` compare the JSON before and after.

**Casts before migration `0007` aren't logged**, so a team whose only casts predate it reloads with an empty log and `last_cast: null` although its `patterns` are non-empty. Every cast since is logged, in the same order memory appended it (`id` is an identity column, and casts for one team are written under its entry lock), so a restart reproduces the log exactly.

## Wrong scans and poisoned attempts
How a mistake plays out, end to end.

**The server decides.** In `Scan`, under the team's lock, after the rejections (no selection, poisoned, waypoint already in): `wrong := !FitsShape(selectedShape, stack ∪ {w})`. `FitsShape` is true if some pattern of the shape contains every waypoint in the set ([`internal/atlas/README.md`](../atlas/README.md)).

**What a wrong scan does:**

| Where | Effect |
|---|---|
| Response | `200`, `outcome: "wrong_scan"`; `state.stack` ends with the scan, `"wrong": true`; `state.poisoned: true` |
| Memory | Pushed onto the stack with `Wrong: true`; its waypoint is added to visited `waypoints` |
| Postgres | `team_scan` +1 with `wrong = true` (`insertScan`) |
| Event | `team_state` to every member, the same `View` as the response |
| Later scans | `409 selection_poisoned`, by anyone, of any waypoint (the wrong one included): nothing written, nothing published |
| Select (same or another shape) | Purges the stack: `poisoned: false`, empty stack; the team scans again |
| Clear | Purges the stack and the selection |
| Restart of the backend | The `wrong` column rebuilds the flag: still poisoned |

**At most one wrong scan per attempt**, always the top of the stack: once one is pushed, every later scan is refused before it could be checked. So the stack never holds more than the pattern's points plus one.

**What the client shows** (`frontend/src/routes/play/+page.svelte`): on `wrong_scan`, a message that the point doesn't match any pattern of the selected type with the points already scanned; the stack lists the wrong scan in red, with who scanned it; while `poisoned`, a banner with a **restart attempt** button (which reselects the same shape) and the scan form disabled.

## Walkthroughs
Team `482913`, members A–F, 2025 seed (square = `1001 1002 1007 1008` = indexes 0, 1, 6, 7, bitmask 195; `square` = shape 2), config.toml as shipped. Times are `mm:ss` after 12:00. Stacks are written bottom → top as `waypoint/member@time`; a wrong scan is marked ✗.

### 1. A square, with every rejection
| Time | Request | Check | Stack after | Postgres after | Response |
|---|---|---|---|---|---|
| 00:00 | F: select `square` | team IN_GAME, shape known | empty | `team_selection(square, F, after_scan_id = latest)` | `200`, `selection.shape = "square"` |
| 00:01 | A: scan `1001` | `{1001}` (bitmask 1) is inside some square | `1001/A@01` | `team_scan` +1 | `scan_added` |
| 00:02 | A: scan `1001` (a retry) | **1001 is already in the stack** | unchanged | unchanged | `409 waypoint_already_scanned` |
| 00:03 | B: scan `1001` | **1001 is already in the stack** | unchanged | unchanged | `409 waypoint_already_scanned` |
| 00:05 | A: scan `1002` | the same member again is fine; `{1001,1002}` = 3 is inside square 195 | `1001/A 1002/A@05` | `team_scan` +1 | `scan_added` |
| 00:10 | C: scan `1007` | set 67 fits the square; 67 is also a **triangle** (shape 4 ≠ 2), so no completion | `… 1007/C@10` | `team_scan` +1 | `scan_added` |
| 00:12 | A: scan `1008` | set 195 = square, shape 2 = selection | **purged**; selection cleared; cast log + square 195 by A @12 | `team_scan` +1, `team_pattern` +1, `team_cast` +1, `team_selection` deleted | `pattern_completed`, `pattern ["1001","1002","1007","1008"]`, `last_cast.cast_by` = A |
| 00:13 | A: scan `1008` (a retry) | no selection | unchanged | unchanged | `409 no_shape_selected` |

### 2. A wrong scan poisons the attempt, and a reselect restarts it
| Time | Request | Check | Stack after | `poisoned` | Response |
|---|---|---|---|---|---|
| 00:00 | F: select `square` | | empty | false | `200` |
| 00:01 | A: `1001` | inside a square | `1001/A@01` | false | `scan_added` |
| 00:02 | B: `1002` | inside a square with 1001 | `1001/A 1002/B@02` | false | `scan_added` |
| 00:03 | C: `1003` | no square has 1001, 1002 *and* 1003 | `1001/A 1002/B 1003/C@03 ✗` | **true** | `200 wrong_scan`; every phone gets the `team_state` |
| 00:04 | D: `1007` | **poisoned** | unchanged | true | `409 selection_poisoned` |
| 00:05 | C: `1003` (a retry) | **poisoned** (checked before "already in") | unchanged | true | `409 selection_poisoned` |
| 00:06 | F: select `square` | reselect always purges | empty (1003 gone) | false | `200` |
| 00:07–10 | A, B, C, D: `1001 1002 1007 1008` | exact square | **purged** | false | `pattern_completed` |

Postgres after 00:03: `team_scan` row for `1003` with `wrong = true`. A restart between 00:03 and 00:06 reloads the same poisoned stack. `1003` stays in visited `waypoints` after the purge.

### 3. No time limit
F selects `square` at 00:00. A scans `1001` at 00:00, B `1002` at 01:00:00 (an hour later), C `1007` at 02:00:00. At 10:00:00 the stack is still `1001/A@00:00 1002/B@01:00:00 1007/C@02:00:00`: nothing ages out, no event is sent with nobody acting. D scans `1008` at 10:00:00: `pattern_completed`.

### 4. Reselecting purges
F selects `square`; A, B, C scan `1001 1002 1007`. F selects `square` again: the stack is purged (`after_scan_id` moves past those three rows). D scans `1008`: `scan_added`, stack `1008/D` only. A may scan `1001` at once (the earlier scan of it is gone).

### 5. One member scans a whole pattern
F selects `square`. D scans `1001`, `1002`, `1007`, `1008` in turn: `scan_added` three times, then `pattern_completed`, `last_cast.cast_by` = D.

### 6. The cast log after a cast and a repeat
| Time | Request | `last_cast` after | `GET /api/teams/me/casts` | `patterns` | `team_cast` rows | `team_pattern` rows |
|---|---|---|---|---|---|---|
| (start) | | `null` | `[]` | `[]` | 0 | 0 |
| 00:12 | A completes square `1001 1002 1007 1008` (walkthrough 1) | `{"pattern":["1001","1002","1007","1008"],"shape":"square","cast_by":A,"cast_at":"…12:00:12Z","already_completed":false,"damage":107}` | `[that cast]` | `[[1001,1002,1007,1008]]` | 1 | 1 |
| 01:00 | F selects `triangle` | **unchanged** (a selection doesn't clear it) | unchanged | unchanged | 1 | 1 |
| 02:00 | F selects `square` again | unchanged | unchanged | unchanged | 1 | 1 |
| 02:01–02:04 | A, B, C, D scan the same square; D's scan completes it | `{…,"cast_by":D,"cast_at":"…12:02:04Z","already_completed":true}` | `[02:04 cast (already_completed: true), 00:12 cast]` | unchanged (counted once) | 2 | 1 (`ON CONFLICT DO NOTHING`) |
| (restart) | | the 02:04 cast | the same two, same order | unchanged | 2 | 1 |

`ScanResult.already_completed` and `last_cast.already_completed` agree for the scan that cast.

## The write-through
Every write in `write.go` (`Select`, `ClearSelection`, `Scan`) runs these steps, marked in the code (`PublishTeam` is not a write; see the table):
1. **lock** the team's entry (`lockTeam`)
2. **validate**:
   - first the atlas lookup (shape or waypoint ID) and the team, **before** the lock (an `IN_GAME` team's status never goes back). This is AGENTS.md §6 variation (a); the code labels it `2. validate, before 1. lock`
   - then, under the lock, membership again (`isMember`: an admin may have removed the caller since the lookup) and the `State` (`2. validate (state)`)
3. **build** the next `State` (a copy; the cast log copy-on-write)
4. **persist**: one `db.go` call (`insertScan`, `insertCompletingScan`, `upsertSelection` or `deleteSelection`), detached from the request and capped at **5 s** (`storage.write_timeout`)
5. **apply**: `entry.cur.Store(&next)`
6. **publish** `team_state` to every member, still holding the lock so a team's events are in order

| Write | `db.go` call (step 4) |
|---|---|
| `Select` | `upsertSelection` |
| `ClearSelection` | `deleteSelection` |
| `Scan`, no completion (wrong included) | `insertScan` |
| `Scan` that casts | `insertCompletingScan` (`team_scan`, `team_pattern`, `team_cast`, delete `team_selection`, one transaction) |
| `PublishTeam` (start hook) | none: not a write. Only steps 2, 1 and 6; the state is published as it is |

**On a failed persist:** `reconcile` reloads the team from Postgres (Postgres wins), publishes the result, and returns the error, which becomes a `500`. Nothing from the failed write is applied.

**Rejections and no-ops return before step 4:** every `409` (`selection_poisoned` included) and clearing when there's nothing to clear. Nothing is written and nothing is published. A **wrong scan is not a rejection**: it's written and published. Reselecting is **not** a no-op: it always purges, writes and publishes.

## API
All routes need `Authorization: Bearer <token>` (`401` without). Bodies over **1 MiB** (`server.max_body_bytes`) get `400 invalid_body`.

| Method | Path | Body | Response payload |
|---|---|---|---|
| GET | `/api/game/shapes` | none | `Game` |
| GET | `/api/teams/me/state` | none | `View` |
| GET | `/api/teams/me/casts` | none | `[CastView]`, newest first |
| PUT | `/api/teams/me/state/selection` | `{"shape": "square"}` | `View` |
| DELETE | `/api/teams/me/state/selection` | none | `View` |
| POST | `/api/teams/me/state/scans` | `{"waypoint_id": "1007"}` | `ScanResult` |

**`View`** (also the data of the `team_state` live event), here for a poisoned attempt:
```json
{
  "team_id": "482913",
  "waypoints": ["1001", "1002", "1003"],
  "patterns": [["1003", "1004", "1005", "1006"]],
  "shapes": ["square"],
  "selection": {"shape": "square", "selected_by": "6f1c…", "selected_at": "2026-10-17T12:00:00Z"},
  "last_cast": {"pattern": ["1003", "1004", "1005", "1006"], "shape": "square", "cast_by": "9a2b…", "cast_at": "2026-10-17T11:58:40Z", "already_completed": false, "damage": 107},
  "stack": [
    {"waypoint_id": "1001", "user_id": "9a2b…", "scanned_at": "2026-10-17T12:00:01Z", "wrong": false},
    {"waypoint_id": "1002", "user_id": "c3d4…", "scanned_at": "2026-10-17T12:00:02Z", "wrong": false},
    {"waypoint_id": "1003", "user_id": "e5f6…", "scanned_at": "2026-10-17T12:00:03Z", "wrong": true}
  ],
  "poisoned": true
}
```
- `selection` is `null` when nothing is selected.
- `last_cast` is the team's newest cast, or `null` if it has none (or cast only before migration `0007`). `pattern` is its sorted waypoint IDs, `shape` its shape ID, `cast_by` the member whose scan completed it, `cast_at` that scan's time, `already_completed` true if the team had cast this pattern before (progress didn't grow), and `damage` is what the cast spell deals (`pattern.damage`). It stays until the next cast: selecting or clearing doesn't remove it.
- `stack` is the attempt's scans, bottom (oldest) first; `[]` when empty. `wrong` is true for the scan that didn't fit (at most one, always the top).
- `poisoned` is true while the stack holds a wrong scan: scans are refused with `selection_poisoned` until a reselect.

**Cast log** (`GET /api/teams/me/casts`), after walkthrough 6:
```json
[
  {"pattern": ["1001", "1002", "1007", "1008"], "shape": "square", "cast_by": "<D>", "cast_at": "2026-10-17T12:02:04Z", "already_completed": true, "damage": 107},
  {"pattern": ["1001", "1002", "1007", "1008"], "shape": "square", "cast_by": "<A>", "cast_at": "2026-10-17T12:00:12Z", "already_completed": false, "damage": 107}
]
```
- Newest first; `[]` before the first cast, or for a team in the lobby.
- Read from memory, like every other read. Not pushed on the stream: `team_state.last_cast` carries each new cast, so a client loads the log once and prepends each new `last_cast` it sees (by `cast_at`), which is what `/play` does.

**`Game`** (`GET /api/game/shapes`), with the 2025 seed and config.toml as shipped:
```json
{
  "shapes": [
    {"id": "collinear", "name": "Collinear", "patterns": 9,   "points": [3], "damage": {"min": 65, "max": 106}},
    {"id": "lightning", "name": "Lightning", "patterns": 77,  "points": [4], "damage": {"min": 86, "max": 131}},
    {"id": "square",    "name": "Square",    "patterns": 18,  "points": [4], "damage": {"min": 93, "max": 131}},
    {"id": "tree",      "name": "Tree",      "patterns": 99,  "points": [4], "damage": {"min": 84, "max": 129}},
    {"id": "triangle",  "name": "Triangle",  "patterns": 156, "points": [3], "damage": {"min": 60, "max": 113}}
  ],
  "max_pattern_points": 6
}
```
- `shapes`: every selectable shape (the client's "types"), in index order (by ID). `id` is what `PUT …/selection` takes; `name` is for display; `patterns` is how many valid patterns the shape has; `points` is the distinct sizes of those patterns, ascending (a shape mixing 3- and 4-point patterns shows `[3, 4]`); `damage` is the minimum and maximum damage across the shape's patterns.
- `max_pattern_points`: **6** (`game.max_pattern_points`).
- Any signed-in user may read it, in a team or not. It's built once at startup and never changes while the backend runs; no live event carries it.

**`ScanResult`:**
```json
{"outcome": "pattern_completed",
 "pattern": ["1001", "1002", "1007", "1008"],
 "already_completed": false,
 "state": { …View… }}
```
`outcome` is one of:
- `scan_added`: the scan was pushed; no pattern yet.
- `wrong_scan`: the scan was pushed, but its waypoint, with the stack, isn't part of any pattern of the selected shape. `state.poisoned` is true; tell the player and offer a restart.
- `pattern_completed`: `pattern` is set; `already_completed` is true if the team had completed this pattern before. The stack and selection are purged, and `state.last_cast` is the new cast.

**Errors** use the envelope, e.g. `409 {"success": false, "code": "selection_poisoned", "message": "a scan didn't fit any pattern of this shape; select the shape again to start over"}`.

| Error | Status | Code |
|---|---|---|
| `ErrNotInTeam` | 404 | `not_in_team` |
| `ErrNotInGame` | 409 | `not_in_game` |
| `ErrUnknownWaypoint` | 404 | `unknown_waypoint` |
| `ErrUnknownShape` | 404 | `unknown_shape` |
| `ErrNoShapeSelected` | 409 | `no_shape_selected` |
| `ErrWaypointAlreadyScanned` | 409 | `waypoint_already_scanned` |
| `ErrSelectionPoisoned` | 409 | `selection_poisoned` |
| a body that doesn't decode | 400 | `invalid_body` |

**Live events:** `team_state` with the `View`, sent:
- to every member after every change: select, clear, accepted scan (wrong included), cast, and `reconcile` (never after a rejection or no-op)
- to every member right after the `IN_GAME` `team` event when the leader starts (`PublishTeam`, through teams' `Rules.OnStart`): the empty state
- right after `team` when a member connects to `GET /api/events`

Nothing is ever published with nobody acting: there are no timers. Every trigger, the ordering guarantees and the wire format: [`internal/server/README.md`, "The event stream"](../server/README.md#the-event-stream).

## Rules and cases

### Loading at startup
The backend refuses to start if any of these hold:

| Case | Error |
|---|---|
| `config.toml` is invalid (see `internal/platform/config`), e.g. a missing key, or `max_pattern_points` outside 1–15 | from `config.Load`, before anything loads |
| The content is invalid (bad waypoint IDs, too many items, unknown, empty, oversized, duplicate or nested patterns, missing damage) | from `atlas.Load`, before this `Load` runs: every case, in order, is in [`internal/atlas/README.md`](../atlas/README.md), "Loading at startup" |
| Reading the per-team tables fails | `load team state: …` |
| Stored visited waypoints name a waypoint the atlas doesn't have | `team 482913 state: unknown waypoint "1012"` |
| A `team_pattern` key the atlas doesn't have | `team 482913 state: unknown pattern 400` |
| A selection's shape the atlas doesn't have | `team 482913 state: selection: unknown shape "hexagon"` |
| A `team_cast` row's pattern the atlas doesn't have | `team 482913 state: cast: unknown pattern 400` |
| A stack scan's waypoint the atlas doesn't have | `team 482913 state: stack scan: unknown waypoint "1012"` |

### Select: `PUT /api/teams/me/state/selection`
Checked in this order:

| Case | Result |
|---|---|
| Body not `{"shape": string}`, unknown fields, empty, or over 1 MiB | `400 invalid_body` |
| Shape unknown | `404 unknown_shape` |
| Caller not in a team | `404 not_in_team` |
| Team not `IN_GAME` | `409 not_in_game` |
| Any member selects (not just the leader) | Allowed |
| Same shape already selected | `200`: **stack purged** (a wrong scan included: `poisoned: false`), selection re-stamped with the caller and now; written; event |
| A different shape, or nothing selected | `200`: selection set; stack purged; written; event |
| Shape already completed before | Allowed; completing another of its patterns adds the pattern |

### Clear: `DELETE /api/teams/me/state/selection`
| Case | Result |
|---|---|
| Caller not in a team | `404 not_in_team` |
| Team not `IN_GAME` | `409 not_in_game` |
| Nothing selected | `200`, no-op: nothing written, no event |
| Otherwise | `200`: selection and stack removed (a wrong scan included); visited waypoints, completions and the cast log kept; event |

### Scan: `POST /api/teams/me/state/scans`
Checked in this order. Every rejection changes nothing, isn't logged and sends no event.

| Case | Result |
|---|---|
| Body not `{"waypoint_id": string}`, empty, or over 1 MiB | `400 invalid_body` |
| Waypoint unknown | `404 unknown_waypoint` |
| Caller not in a team | `404 not_in_team` |
| Team not `IN_GAME` | `409 not_in_game` |
| No shape selected (including right after a completion) | `409 no_shape_selected` |
| The stack holds a wrong scan (poisoned), whatever the waypoint, whoever scans | `409 selection_poisoned` |
| Waypoint already in the stack, by anyone (a retry included) | `409 waypoint_already_scanned` |
| Caller already has scans in the stack | Allowed: any member may scan any number of waypoints |
| Any time since the selection or the last scan, however long | Allowed: there's no time limit |
| The waypoint, with the stack, isn't part of any pattern of the selected shape | `200 wrong_scan`: pushed with `wrong: true`, `poisoned: true`; logged with `wrong = true`; event |
| Stack plus this scan is exactly a pattern of the **selected** shape | `200 pattern_completed`: pattern and shape marked; **stack and selection purged**; cast appended to the log and `last_cast` (`already_completed: false`); `team_cast` +1 |
| ... that pattern was already completed | `pattern_completed` with `already_completed: true`; progress unchanged; cast appended (`already_completed: true`); `team_cast` +1, `team_pattern` unchanged |
| Stack plus this scan is a pattern of **another** shape | Not completed → `scan_added` |
| Otherwise | `scan_added` |
| 1-point pattern | Completes on its only scan |
| 6-point pattern | Completes on the sixth scan (the stack never needs its seventh slot for correct scans) |
| Any accepted scan (wrong included) | Logged in `team_scan` (with `wrong`); waypoint added to visited `waypoints` |
| Postgres write fails | `500`; state reloaded from Postgres; nothing applied |

### Start: `PublishTeam`, no request (teams' `Rules.OnStart`)
Checked in this order:

| Case | Result |
|---|---|
| No team with that ID | Nothing published |
| Team not `IN_GAME` (lobby) | Nothing published |
| Team `IN_GAME` (always, when called by teams' `Start`) | `team_state` with the current state to every member: just after a start, the empty state. Memory is unchanged except that an empty entry is created if the team had none (reads already report that same empty state); Postgres untouched |
| Called again, or after a write | The current state again: a full snapshot, so a repeat is harmless (`TestPublishTeam`) |

### Cast log: `GET /api/teams/me/casts`, and `last_cast`
| Case | Result |
|---|---|
| Caller not in a team | `404 not_in_team` |
| Team in the lobby, or never cast | `200 []`; `last_cast: null` |
| A cast | Appended to the log; it's the first entry of `GET …/casts` and is `last_cast` |
| A repeat cast of the same pattern | Appended again with `already_completed: true`; `last_cast` is it |
| Select, reselect, clear, wrong scan | Log and `last_cast` unchanged |
| Restart | Every `team_cast` row, in `id` order: the same log, the same `last_cast` |
| Casts only before migration `0007` | Not in the log; `last_cast: null` after a restart |

### Game rules: `GET /api/game/shapes`
| Case | Result |
|---|---|
| No token, or a bad one | `401 missing_token` / `401 invalid_token` (auth middleware) |
| Signed in, in a team or not, any team status | `200` with the `Game` |
| Content or `config.toml` changed | Not seen until restart, like all content |

### Get: `GET /api/teams/me/state`
| Case | Result |
|---|---|
| Caller not in a team | `404 not_in_team` |
| Team in the lobby or with no activity | `200`, empty lists, `selection` and `last_cast` null, `poisoned: false` |
| Poisoned attempt | `200`, the stack with its wrong scan marked, `poisoned: true` |

## Concurrency and locking
- **Writes to one team are serialised** on its `entry.mu`. Different teams run in parallel.
- **Two members scan the same waypoint at once:** exactly one is pushed; the other gets `waypoint_already_scanned`.
- **One member scans two waypoints at once:** both are pushed, in lock order (if both fit).
- **A phone resends one scan many times at once:** exactly one is pushed and logged; the rest get `waypoint_already_scanned`.
- **A wrong scan races a correct one:** whichever takes the lock first is pushed first. If the correct one wins, the wrong one is then checked against a bigger stack (still wrong: `FitsShape` only gets stricter as the set grows) and poisons. If the wrong one wins, the correct one gets `selection_poisoned`. Either way every phone ends poisoned, with the same stack (`TestSimWrongScanReachesEveryone`).
- **A scan and a selection race:** whichever takes the lock first wins. If the selection comes first, the stack is purged and the scan is the first of the new stack.
- **Two casts can't race:** a cast purges the selection under the lock, so the next scan gets `no_shape_selected`. The log is appended under the lock, so its order is the lock order, which is also `team_cast.id` order.
- **Readers of the cast log** load a snapshot and range over its `Casts` slice with no lock: a writer never writes into a slice a snapshot holds (copy-on-write), so readers can't see a half-appended log.
- **Lock order:** `entry.mu`, then `Store.mu` (only to create an entry). `Store.mu` is never held during I/O.
- **The start hook.** `PublishTeam` runs inside teams' `Start`, which holds the team's teams entry lock; it takes this team's `entry.mu`. So the lock order across the two use cases is teams entry → teamstate entry. teamstate never takes a teams entry lock (`ByID` and `ForUser` take only teams' index lock, briefly), so this can't deadlock. Holding teams' lock is what makes the start's `team` arrive before its `team_state`. A write that slips in between teams' apply and the hook (a member who scans before seeing the start) publishes its own `team_state` first; the hook then publishes the same, current state again.
- **The team itself is read** through `Teams` without the team's lock. Its status is safe to read that way: writes need `IN_GAME`, which never goes back. Its roster isn't frozen: an admin can remove a member (teams' `RemoveMember`), so membership is checked again under `entry.mu` (`isMember`).
- **An admin's removal versus the removed player's write.** teams applies the removal (the roster in `ByID` no longer has them), publishes, then calls `RosterChanged`, which takes this team's `entry.mu`. A write by the removed player either:
  - holds `entry.mu` and passed `isMember` before the removal was applied: it finishes first and counts (it happened before the removal), and `RosterChanged` waits for it; or
  - checks `isMember` after the removal was applied: refused, `404 not_in_team`.

  So no write by a removed player is accepted once the removal is visible, and none is half-applied. Their earlier scans stay in the stack and still count toward a pattern (`TestAdminRemoveFromStartedTeam`, `TestAdminRemoveRacesScan`). After `RosterChanged`, publishes go to the remaining members only. Lock order is the same as the start hook's: teams entry → teamstate entry.
- **The memory checks are the only enforcement** of the stack rules (one scan per waypoint per attempt, nothing after a wrong scan): they depend on the selection's `after_scan_id` window, which a unique constraint can't express. They all run under `entry.mu`, and Postgres has one writer (the instance lock in `platform/postgres`).

## Failure and recovery
| Case | Result |
|---|---|
| Postgres times out or drops during a write | `500`. `reconcile` re-reads the team (the write may or may not have committed) and memory matches Postgres |
| Memory and Postgres disagree (rows edited by hand) | The next write that notices (e.g. `deleteSelection` finding no row) fails, reconciles, and members get the corrected state |
| Postgres unreachable | Reads keep working from memory (the cast log included); writes fail |
| Backend restarts | Progress, selections, stacks (with their `wrong` flags, so `poisoned` too), the cast log and `last_cast` reload exactly. Nothing depends on how long the backend was down |
| `config.toml` changes between restarts | A larger `game.max_pattern_points` loads the same stacks. A smaller one refuses content with bigger patterns (`atlas.Load`); a stored stack longer than the new capacity loads only its newest scans. Don't change it mid-game |
| Content edited while running | Not seen until restart (the atlas loads once). Don't: if the new content drops a waypoint, pattern or shape a team's stored state refers to (its cast log included), the next start refuses. See [`internal/atlas/README.md`](../atlas/README.md) |

## Limits and how to change them
| Limit | Where | To change |
|---|---|---|
| 6 points per pattern; stack capacity 7 | `config.toml` `game.max_pattern_points` (1–15) | Edit and restart. Content with bigger patterns is refused at startup (by `atlas.Load`) |
| 16 slots in a `Stack` | `config.MaxPatternPoints` (15) + 1 | Fixed array size; raise the constant to allow bigger patterns. Every snapshot grows by 56 bytes per slot |
| Cast log length | none | Unbounded; 56 bytes per cast in memory, and a cast copies the team's log once |
| 64 waypoints, 64 shapes, 2048 patterns | the atlas's set types (`atlas.MaxWaypoints`, `atlas.MaxShapes`, `atlas.MaxPatterns`) | See [`internal/atlas/README.md`](../atlas/README.md), "Limits and how to change them". `State` holds one of each set, so a wider set makes every snapshot bigger |

## Not decided yet
- **⚠ QR verification.** `POST …/scans` accepts the raw 4-digit waypoint ID, so a player can claim a scan without being there. This must be replaced before the event (rotating or signed QR codes, verified server-side).
- **Whether the leader alone should select.** Currently any member can, so any member can restart a poisoned attempt.
- **Score.** Casts carry `damage`, but nothing sums it yet; the cast log is the raw record a score would be computed from.

## Tests
- The content lookups teamstate relies on (`PatternFor`, `FitsShape`, `SetOf`, validation, the seed loading, `BenchmarkLookups`) are tested in the atlas: [`internal/atlas/README.md`](../atlas/README.md), "Tests".
- `tests/stack_test.go` (no database):

  | Test | Covers |
  |---|---|
  | `TestStack` | `NewStack(3)` has `Cap` 4 (a pattern's points plus one wrong scan); bottom-to-top order; a wrong scan on top keeps its flag; `Push` leaves the original alone; a push onto a full stack panics |

- `tests/store_test.go` (real Postgres, schema `teamstate_test`, the 2025 seed loaded through `atlas.Load`, config.toml's rules, a fake clock). The fake `clock` implements `teamstate.Clock`: `Now` returns the fake time and `at(d)` sets it to `start` + `d`. Every helper (`scanAt`, `selectAt`) sets the clock first, so stored times are predictable. Times are written with `tick` (seconds); they only order and stamp events. The tests use up to 6 members (`minTestTeam`); with a smaller `teams.size` in `config.toml` they **skip** rather than fail:

  | Test | Covers |
  |---|---|
  | `TestWritesRequireGame` | input validation and lobby refusals for every write |
  | `TestScanNeedsSelection` | scanning before selecting changes nothing |
  | `TestSquareCasts` | walkthrough 1: a waypoint rejection changes nothing, one member scans two waypoints, the triangle that doesn't count, completion, `last_cast`, no selection after |
  | `TestOneMemberScansWholePattern` | walkthrough 5: one member scans every point of the square |
  | `TestNoTimeLimit` | walkthrough 3: scans hours apart stay in the stack and still complete |
  | `TestEverySelectionPurges` | walkthrough 4; reselect and select-another both purge |
  | `TestRescanIsRejected` | the same member's retry and another member's scan of a stacked waypoint: rejected, not logged, no event |
  | `TestConcurrentRescansAddOneScan` | 8 simultaneous copies → one pushed, seven `waypoint_already_scanned` |
  | `TestWrongScanIsReported` | a waypoint in no square: `wrong_scan`, marked wrong in the stack, `poisoned`, logged; later scans by the scanner and a teammate get `selection_poisoned` |
  | `TestWrongScanPoisonsSelection` | walkthrough 2: a poisoned attempt refuses the rest of the square; a reselect clears it and the square then completes |
  | `TestCompletionPurges` | stack and selection purged; rescans need a new selection |
  | `TestOutOfOrderScanning` | a square completes when scanned in reverse order (a pattern is an unordered set) |
  | `TestSinglePointPattern` | a 1-point pattern |
  | `TestSixPointPatternAtCapacity` | a 6-point pattern (`game.max_pattern_points`) completes on the sixth scan |
  | `TestCompletingAgain` | `already_completed`, progress counted once |
  | `TestClearSelection` | clear with and without a selection; waypoints kept |
  | `TestRestartRestoresEverything` | restart reproduces the JSON view exactly; rescans still rejected |
  | `TestConcurrentScans` | two members at one waypoint (one accepted); one member at two waypoints (both accepted) |
  | `TestFailedWriteReconciles` | reload after a hand-edited database |
  | `TestReadsDoNotTouchDatabase` | reads with the database closed; a failed write leaves memory alone |
  | `TestStateIsPublished` | live events reach teammates |
  | `TestPublishTeam` | `PublishTeam` sends the empty state to every member of a started team; after a selection it sends the current state (square selected) without changing it; nothing for an unknown team or a lobby team |
  | `TestCastLog` | walkthrough 6: no casts before; `last_cast` (pattern, shape, `cast_by`, `cast_at`, `damage`) after each of two casts, the repeat marked `already_completed`; a new selection keeps them; `CastsForUser` newest first; two `team_cast` rows; a restart reloads the same log (JSON equal) |

- `tests/publish_bench_test.go`: **`BenchmarkTeamStatePublish`**, parallel `PublishTeam` calls across 1,666 `IN_GAME` teams (10,000 players at `teams.size` 6; an in-memory `Teams`, so no team rows), every member with an open subscription. It measures the lock, encoding once and queueing for each member: about **1.0 µs** a publish on a 10-core laptop, down from 4.4 µs before the sharded hub and roster cache (`internal/platform/README.md`, "Performance"). Measured before the cast log and `poisoned` were added to the `View`; a team with no casts encodes the same size plus `"poisoned":false`.
- `internal/server/tests/teamstate_test.go`:

  | Test | Covers |
  |---|---|
  | `TestSelectAndScanRoutes` | every route and error code over HTTP: `not_in_game`, `not_in_team`, `invalid_body` (the body size limit included), `unknown_shape`, `unknown_waypoint`, `no_shape_selected`, `waypoint_already_scanned`; one member scanning two waypoints; a square completing; a wrong scan (`wrong_scan`, `poisoned`, `stack[].wrong`), then `selection_poisoned`, then a reselect clearing it |
  | `TestCastLogRoute` | `GET /api/teams/me/casts`: `404 not_in_team`, `[]` before any cast, then two casts newest first with `already_completed`, `shape` and `damage` |
  | `TestGameShapes` | `GET /api/game/shapes`: `401 missing_token` without a token; the seed's 5 shapes, `square` with 18 patterns of 4 points, a valid damage range per shape, `max_pattern_points` from `config.toml`, and no `scan_window_ms` |

- `internal/server/tests/simulation_test.go`: simulated phones, each with its own request and response delays and its own event stream delivered after that delay:

  | Test | Covers |
  |---|---|
  | `TestSimTeamCompletesUnderLatency` | four players scan the square at once with delays up to 150 ms each way: exactly one `pattern_completed`; every screen ends with an empty stack and the square completed |
  | `TestSimRaceSameWaypoint` | two players scan the same waypoint at once: one `200`, one `409 waypoint_already_scanned`; every screen converges on a one-scan stack |
  | `TestSimWrongScanReachesEveryone` | a wrong scan racing a teammate's correct one: the wrong one gets `wrong_scan`, the other is accepted or gets `selection_poisoned`; every phone sees `poisoned`; a reselect clears it on every phone |

- `internal/server/tests/admin_test.go`:

  | Test | Covers |
  |---|---|
  | `TestAdminRemoveFromStartedTeam` | after an admin removes a member of a started team: their stream gets `no_team` and no further `team_state`; their scan, selection and state read get `404 not_in_team`; the rest complete a square using the removed player's earlier scan |
  | `TestAdminRemoveRacesScan` | a member's scan racing their removal, for every non-leader: the removal always succeeds; the scan is accepted (and stays in the stack) or refused with `not_in_team`, never accepted after the removal. Which wins isn't controlled; the refused path is covered deterministically above |

- `internal/server/tests/events_test.go`: `team_state` on connect; `TestStartPublishesTeamState`: when the leader starts, every open stream gets `team` then the empty `team_state`, in that order.
