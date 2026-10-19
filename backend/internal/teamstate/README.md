# teamstate

What each team does in the game: the shape it has selected, its stack of scans toward a pattern of that shape, the waypoints it has visited, the patterns and shapes it has completed, and its last cast. It also times and publishes the one change that happens without a request: a correct scan going stale.

Every number below that comes from `config.toml` is shown with its current value and its key, e.g. **1 m** (`game.scan_window`). Change the file, not the code.

## Contents
1. [Concepts](#concepts)
2. [The game flow](#the-game-flow)
3. [How each file works](#how-each-file-works)
4. [Data model: memory](#data-model-memory)
5. [Data model: Postgres](#data-model-postgres)
6. [Memory ↔ Postgres](#memory--postgres)
7. [The client countdown and latency](#the-client-countdown-and-latency)
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
| **Stack** | The team's accepted scans since its selection, oldest at the **bottom**, newest on **top**. It only ever holds **one scan per waypoint and one per member**, so at most **6** (`game.max_pattern_points`). |
| **Limit** | **65 s**: the scan window **1 m** (`game.scan_window`) plus the grace **5 s** (`game.scan_grace`). The most a pattern's scans may span, bottom to top, and the age at which a *correct* scan goes stale. |
| **Live / stale scan** | A *correct* scan in the stack is **live** while it is at most the limit old (`now − scanned_at ≤ 65 s`), and **stale** after. Stale correct scans don't count. The **expiry timer** (below) drops them from memory and publishes the shorter stack at the first stale microsecond; the next accepted scan also drops them, whichever comes first. |
| **Expiry timer** | At most one per team: a `Clock` timer set for the moment the stack's oldest correct scan goes stale (`scanned_at` + 65 s + 1 µs, `Stack.NextExpiry`). When it fires, `expire` drops the stale scans and publishes `team_state`. None is pending when the stack holds no correct scans (empty, cast, purged, or only wrong scans). |
| **Clock** | Where the store gets time: `Now()` and `AfterFunc(d, f)`. `systemClock` is the real one (`time.Now`, `time.AfterFunc`); tests pass a fake through `Rules.Clock`. |
| **Cast** | A completion: the moment the stack becomes exactly a pattern of the selected shape. Every cast is logged in `team_cast`, repeats included; `team_pattern` keeps only the first completion of each pattern. The client calls it casting a spell. |
| **Spell lifecycle** | What the client shows as a spell, read from `team_state` alone, with no event of its own: it **starts** when `selection` is set, members **finish their part** as `stack` grows (each `stack[].user_id` is a member who scanned; its length is how many), and it **finishes** when `last_cast` changes (a new `cast_at`). |
| **Last cast** | `State.LastCast`, `last_cast` in JSON: the team's newest cast (pattern, shape, who, when, and whether the pattern was already completed). Kept until the next cast; a new selection or a clear doesn't remove it. |
| **Wrong scan** | A scan whose waypoint, with the correct scans already in the stack, isn't part of any pattern of the selected shape. It's still pushed (never rejected), occupies its member's one scan and its waypoint slot, poisons the selection (it can no longer complete), and **never goes stale**: it stays until the selection is purged. The client is never told which scans are wrong. |
| **Visited waypoints** | Every waypoint the team has ever had a scan accepted at, under any selection. |
| **Countdown** | The client's timer, to a deadline on the server's clock: the bottom scan's `scanned_at` + **1 m** (`game.scan_window`), the same on every phone. The phone estimates server time from the stream's `hello`. After the deadline the phone sends no scans. The grace is never shown; it absorbs network delay (see [The client countdown and latency](#the-client-countdown-and-latency)). |
| **Game rules** | What `GET /api/game/shapes` returns: the selectable shapes (the client's "types") with their pattern counts and sizes, plus the window, grace and stack capacity in milliseconds and points. Static while the backend runs. |

**Client words.** The client's **"type"** is our **Shape**, its **"shape"** is our **Pattern**, its **"point"** is our **Waypoint**, and its **"buffer"** is our **Stack**. This README and the code always use the code's words. The full vocabulary, spells included, is in [`internal/atlas/README.md`](../atlas/README.md).

## The game flow
0. **Start.** When the leader starts the team (teams' `Start`), every member's stream gets the `IN_GAME` `team` and then the team's `team_state`, empty: no selection, no stack, no progress. teams calls `PublishTeam` through its `Rules.OnStart` hook, wired in `server.Build`, because teams may not import teamstate (AGENTS.md §4). The play screen has its state at once, without reconnecting.
1. **Select.** Any member selects a shape: `PUT /api/teams/me/state/selection {"shape":"square"}`. **Every selection purges the stack**, reselecting the same shape included.
2. **Scan.** Members scan waypoints: `POST /api/teams/me/state/scans {"waypoint_id":"1001"}`. Checked against the stack's live scans, a scan is **rejected with an error and not pushed** if:
   - its **waypoint is already in the stack**, whoever scanned it, the same member included: `409 waypoint_already_scanned`
   - its **member already has a scan in the stack**: `409 member_already_scanned`

   Otherwise stale correct scans are dropped from the bottom and the scan is pushed on top. A scan whose waypoint doesn't fit the selected shape is pushed anyway, marked **wrong** (see [Wrong scan](#concepts)).
3. **Complete (cast).** The moment the stack is **exactly** one of the selected shape's patterns, the pattern and its shape are marked complete, the cast becomes `last_cast` and is logged in `team_cast`, and **the stack and the selection are purged**. The team selects again for its next attempt. A wrong scan poisons the set, so a selection with one can never complete.
4. **Stale correct scans drop on their own.** A correct scan more than 65 s old stops counting, which frees its waypoint and its member: only that point is rescanned, the rest of the pattern isn't lost. Wrong scans never drop. Because every live correct scan is at most 65 s old and the newest is now, the correct stack always spans at most the limit, bottom to top. **The server drops it and publishes**: the team's expiry timer fires at the first stale microsecond, and every member's screen gets a `team_state` without the scan, with nobody acting.
5. **Give up.** `DELETE /api/teams/me/state/selection` clears the selection and the stack.

**"Part of a pattern" narrows as the team scans.** The team selects a shape, not a pattern, so the first scan is correct if it's in *any* pattern of the shape. Each later scan must keep the correct stack inside at least one pattern of the shape that contains everything already scanned. A scan that doesn't is wrong and is final: reselect to purge it (it never goes stale on its own).

**Why the shape is selected first.** With the 2025 data any three waypoints form a triangle or a collinear pattern, so a team scanning a square passes through a "triangle" after its third scan. Only the selected shape can complete, which is what makes 4-waypoint shapes possible.

**Why the server publishes expiry.** Every client must see the same stack. If each phone hid stale scans by its own clock, phones with skewed clocks would disagree, and a phone would show a scan the server no longer counts. The server owns the rule, so it announces the change (AGENTS.md §17); clients use `expires_at` only for display.

**Retries.** A phone resending a scan gets `409 waypoint_already_scanned`; the app should treat that as "already in" and refresh from the returned state or the live event.

## How each file works

### `api.go`: routes
- **`Routes`** registers five routes behind the auth middleware (see [API](#api)).
- **Handlers:**
  - `shapes` returns `Store.Game()` to any signed-in user, in a team or not.
  - `getMine` returns the caller's team state.
  - `selectShape` decodes `{"shape"}` and calls `Store.Select`.
  - `clearSelection` calls `Store.ClearSelection`.
  - `scan` decodes `{"waypoint_id"}` and calls `Store.Scan`, returning a `ScanResult`.
- **`fail`** maps errors to statuses and codes via the `errorCodes` table; anything unmapped is logged and becomes `500`.
- **⚠** The scan body is the raw waypoint ID, so it doesn't prove presence. See [Not decided yet](#not-decided-yet).

### `read.go`: types and reads
- **Types:**
  - `State`: the in-memory per-team snapshot, made of `Selection`, `Stack`, `StackScan` and `Cast` (see [Data model: memory](#data-model-memory)).
  - `Cast`: one completed pattern (`Active`, `Pattern`, `By`, `At`, `AlreadyCompleted`), held as `State.LastCast`.
  - `View`, `SelectionView`, `CastView`, `StackScanView`: the JSON shapes. `CastView` is `last_cast`: `pattern` (sorted waypoint IDs), `shape`, `cast_by`, `cast_at`, `already_completed`, `damage`.
  - `Game`, `GameShape`: the JSON of `GET /api/game/shapes`, including each shape's `damage` range (`{"min": …, "max": …}`).
- **`Stack` methods** (all on values; none modifies the receiver):
  - `NewStack(cap)` is an empty stack (exported so tests can build one).
  - `Len()` counts scans, stale ones included.
  - `At(i)` is the i-th from the bottom.
  - `Push(sc)` returns the stack with `sc` on top. Scan never pushes onto a full stack (its scans always fit one pattern); if it did, the bottom would be dropped.
  - `Live(now, limit)` returns the correct scans at most `limit` old plus every wrong scan, bottom to top.
  - `NextExpiry(limit)` returns the first instant the oldest correct scan is stale: its `At` + `limit` + **1 µs**, because a scan exactly `limit` old is still live (`Live` uses `≤`) and times have microsecond precision. `ok` is false when the stack holds no correct scans (wrong scans never expire). Used by `scheduleExpiry`.
- **`newGame(atlas, rules)`** builds the `Game` once, in `Load`: for each shape in index order (`ShapeCount`, `Shape(i)`), its ID and name, how many patterns it has (`len(ShapePatterns(i))`), and the distinct sizes of those patterns (`Pattern(p).Waypoints.Len()`), ascending; plus `ScanWindowMS`, `ScanGraceMS` (`Duration.Milliseconds()`, so sub-millisecond parts are dropped) and `MaxPatternPoints` from `Rules`.
- **Reads**, all from memory:
  - `Game()`: the prebuilt `Game`; no lock, it never changes
  - `ForTeam(teamID)` → `State`
  - `ForUser(userID)` → `View`
  - `Snapshot(userID)`: the live event for a new stream
- **`view`** converts indexes and bits to IDs through the atlas (`WaypointIDs`, `Pattern`, `Shape`, `Waypoint`), times to UTC, builds `last_cast` from `State.LastCast` (`null` when not `Active`; the shape is the pattern's `Shape`), and leaves stale correct scans out of `stack` (wrong scans are included, with no flag distinguishing them).

### `write.go`: writes, each written through
- **`Select`**, **`ClearSelection`** and **`Scan`** each follow the [write-through](#the-write-through) steps, numbered in the code. After `6. publish`, still holding the lock, each calls `scheduleExpiry` for the new state.
- **Lock-free checks first** (§6 variation (a)): `Select` resolves the shape with `Atlas.ShapeIndex`, `Scan` the waypoint with `Atlas.WaypointIndex`, both before the team and the lock.
- **`Scan`'s checks**, in order, over the live scans: same waypoint → `ErrWaypointAlreadyScanned`; same member → `ErrMemberAlreadyScanned`; then `Atlas.FitsShape(shape, stack ∪ {w})` decides whether the scan is **wrong** (still pushed, marked `Wrong`). Completion is `Atlas.PatternFor(stack ∪ {w})` (one map lookup) being a pattern whose `Atlas.Pattern(p).Shape` is the selected shape; a wrong scan can't complete because it's in the set. On completion, `Atlas.WaypointIDs` gives `ScanResult.Pattern`, `Atlas.Pattern(p).ID` is the key written to `team_pattern` and `team_cast`, `AlreadyCompleted` is `cur.Patterns.Has(p)`, and `next.LastCast` is set to `Cast{Active: true, Pattern: p, By: caller, At: now, AlreadyCompleted}`; `alreadyCompleted` is passed to `insertCompletingScan`.
- **`inGameTeam`** fetches the caller's team and requires `IN_GAME`.
- **`PublishTeam(teamID)`** sends the team's current state to every member as `team_state`, changing nothing. teams calls it through `teams.Rules.OnStart` (wired in `server.Build`) right after a team starts, so members get the new team's empty state after its `team` event. Steps: `2. validate, before 1. lock` (`Teams.ByID`: the team exists and is `IN_GAME`, else nothing is published), `1. lock` (`lockTeam`, which creates the empty entry if the team had none), `6. publish` (`publish`, which fills the entry's roster cache on this first publish of the started team). Not a write: no build, persist or apply, and it doesn't touch the expiry timer. It takes the entry lock so its event is ordered with the team's writes.
- **Declared here:** the errors, the `Outcome…` constants, and `ScanResult`.

### `db.go`: SQL
Only the per-team tables. The content tables are read by the atlas (`internal/atlas/db.go`).
- **`selectTeams(ctx, teamID, stackSize)`** reads every team's stored state, or one team's (`teamID != ""`), into `teamRows`: distinct visited waypoints, completed patterns, the newest cast (`lastCast`, a `castRow`), selection, and the stack. The newest cast is `SELECT DISTINCT ON (team_id) … FROM team_cast ORDER BY team_id, id DESC`, served by `team_cast_team_id_idx`. Used by `Load` and `reconcile`. The stack query is a `JOIN LATERAL`: for each selection, the team's newest `stackSize` `team_scan` rows with `id > after_scan_id`, returned oldest first, with the `wrong` flag so wrong scans rebuild as never-stale.
- **`insertScan`**: one `team_scan` row, with the `wrong` flag (a scan that doesn't complete anything).
- **`insertCompletingScan`**: one `pgx.BeginFunc` transaction, four statements in order:
  1. `INSERT team_scan`: the scan itself (`wrong` is always false here)
  2. `INSERT team_pattern … ON CONFLICT DO NOTHING`: the first completion only
  3. `INSERT team_cast (team_id, pattern_id, cast_by, cast_at, already_completed)`: every cast, repeats included
  4. `DELETE team_selection`: which also empties the stack
- **`upsertSelection`**: upserts `team_selection` with `after_scan_id` = the team's latest `team_scan.id` (0 if none). That's the purge: earlier scans are no longer after it.
- **`deleteSelection`**: deletes `team_selection`, and fails unless exactly one row went (memory had a selection, so Postgres must too).

### `store.go`: the in-memory layout
- **`Teams`** is what team state needs from the teams use case: `ForUser(userID)` and `ByID(teamID)`.
- **`Atlas`** is what team state needs from the atlas use case, declared here (AGENTS.md §4) and satisfied by `*atlas.Atlas`. The atlas never changes after startup, so every call is lock-free:

  | Method | Teamstate uses it for |
  |---|---|
  | `WaypointIndex(id)` | `Scan`: unknown waypoint → `404 unknown_waypoint`; `stateOf`: a stored stack scan's waypoint |
  | `ShapeIndex(id)` | `Select`: unknown shape → `404 unknown_shape`; `stateOf`: a stored selection's shape |
  | `PatternByID(id)` | `stateOf`: a stored `team_pattern` key, or the newest `team_cast.pattern_id`, → pattern index |
  | `SetOf(ids)` | `stateOf`: stored visited waypoints → `State.Waypoints` |
  | `PatternFor(set)` | `Scan`: is the stack plus this scan exactly a pattern? |
  | `FitsShape(shape, set)` | `Scan`: is the scan **wrong**? |
  | `WaypointIDs(set)` | `view` (visited waypoints, completed patterns, `last_cast.pattern`), `ScanResult.Pattern` |
  | `ShapePatterns(shape)` | `newGame`: each shape's pattern count and sizes |
  | `ShapeCount()` | `newGame`: how many shapes |
  | `Waypoint(i)` | `view`: a stack scan's waypoint ID |
  | `Shape(i)` | `view` (completed shapes, selection, `last_cast.shape`), `newGame` |
  | `Pattern(i)` | `Scan` (the completed pattern's shape and Postgres `ID`), `stateOf` (a completed pattern's shape), `view` (patterns, `last_cast`), `newGame` |
- **`Clock`** is the store's time source, an interface: `Now()` (UTC, truncated to microseconds) and `AfterFunc(d, f) (stop func() bool)` (call `f` in its own goroutine after `d`, at once if `d ≤ 0`; `stop` cancels it if it hasn't run). **`systemClock`** is the real one: `time.Now().UTC().Truncate(time.Microsecond)` and `time.AfterFunc(d, f).Stop`.
- **`Rules`** are the settings from `config.toml`: `ScanWindow`, `ScanGrace`, `MaxPatternPoints`, `WriteTimeout`, and `Clock` (a `Clock`; nil means `systemClock`, tests pass a fake). `Limit()` is window + grace.
- **`Load(ctx, pg, atlas, teams, hub, log, rules)`** reads every team's stored state, checking it against the atlas, and builds the `Game` (`newGame`). It runs after `atlas.Load` (it's handed the `*atlas.Atlas`) and after `teams.Load` (it reads teams through the `Teams` interface). Once **the whole map is built**, it locks each entry in turn and calls `scheduleExpiry`, so stacks from before the restart keep ageing. A timer already due fires at once, and its callback reads the map, which is why arming waits for it to be complete.
- **`stateOf(teamRows)`** rebuilds a `State` from IDs through the atlas, failing on unknown waypoints, patterns or shapes. The newest `team_cast` row becomes `LastCast` (`last cast: unknown pattern N` if the atlas lacks it).
- **`lockTeam`** gets or creates a team's entry and locks it.
- **`persist`** runs a `db.go` write detached from the request, capped at **5 s** (`storage.write_timeout`).
- **`reconcile`** reloads one team's state from Postgres after a failed write, publishes it and re-arms its timer (`scheduleExpiry`). If the reload fails too, memory and the timer stay as they were.
- **`scheduleExpiry(e, teamID, st)`**, called with `e.mu` held after every apply (`Select`, `ClearSelection`, `Scan`, `reconcile`, `expire`) and by `Load`: cancels the pending timer (`e.stopExpiry`), then, if `st.Stack.NextExpiry(limit)` is ok, arms `clock.AfterFunc(at − now, func() { expire(teamID) })` and keeps its `stop` in `e.stopExpiry`. So each team has **at most one** timer, for its oldest correct scan.
- **`expire(teamID)`**, the timer's callback, in its own goroutine. Steps, marked in the code like a write:
  1. lock the entry (`lockTeam`)
  2. validate: `Live(now, limit)`. If nothing is stale (a write purged or rebuilt the stack since the timer was set), re-arm and return: **nothing published**
  3. build: a new stack of the live scans (wrong ones included)
  5. apply, 6. publish `team_state`, then `scheduleExpiry` for the next oldest correct scan

  There's no step 4: it's a **memory-only change** (AGENTS.md §6). Postgres never stores the stack, only `team_scan`; `Load` rebuilds it, and the same scans are stale there by their stored times.
- **`publish(e, teamID, st)`**, called with `e.mu` held, sends `team_state` to every member. The recipients are **`e.members`**, the team's roster cached on the first publish that finds the team `IN_GAME` (its roster is frozen from then on, so the cache can't go stale). Until then it reads the roster through `Teams.ByID` each time. So a publish reads nothing outside the entry: no index lock, no lookup.
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
  states   map[teamID]*entry         created on a team's first write (or expiry)
  clock    Clock                     Rules.Clock, or systemClock

entry
  mu          sync.Mutex             writers (and expire) to this team queue here
  cur         atomic.Pointer[State]  the current snapshot; readers load it lock-free
  stopExpiry  func() bool            cancels the pending expiry timer; nil if none.
                                     Guarded by mu
  members     []uuid.UUID            the user IDs events go to: the frozen roster,
                                     cached by publish once the team is IN_GAME;
                                     nil until then. Guarded by mu

State                                (never modified once stored)
  Waypoints  atlas.WaypointSet  uint64      visited waypoints, bit i = waypoint index i
  Patterns   atlas.PatternSet   [32]uint64  completed patterns, bit i = pattern index i
  Shapes     atlas.ShapeSet     uint64      completed shapes, bit i = shape index i
  Selection  Selection
    Active   bool                    false → no selection; other fields zero
    Shape    int                     the shape's index in the atlas
    By       uuid.UUID               the member who selected
    At       time.Time               when
  Stack      Stack                   scans since the selection
  LastCast   Cast                    the newest cast; kept until the next one
    Active           bool            false → never cast; other fields zero
    Pattern          int             the pattern's index in the atlas
    By               uuid.UUID       the member whose scan completed it
    At               time.Time       when
    AlreadyCompleted bool            the team had cast this pattern before

Stack                                a value; Push returns a new one
  scans    [16]StackScan             circular buffer; 16 = config.MaxTeamSize,
                                     only the first Cap slots are used
  bottom   int                       slot of the oldest scan
  n        int                       0…Cap scans held
  Cap      int                       game.max_pattern_points (6)

StackScan
  Waypoint int                       the waypoint's index in the atlas
  UserID   uuid.UUID                 who scanned
  At       time.Time                 when
  Wrong    bool                      the scan didn't fit the selected shape;
                                     never ages out; not exposed in View
```
- **Size:** a `State` is a fixed 1312 bytes (`Stack` 920, `Selection` 56, `Cast` 64; measured with `unsafe.Sizeof` on a copy of the layout, linux/amd64), so a new snapshot is a plain struct copy with no heap allocation for its contents. 10,000 teams' states are about 13 MB.
- **The sets are the atlas's types** (`atlas.WaypointSet`, `atlas.PatternSet`, `atlas.ShapeSet`), over the atlas's indexes. How they're laid out and why: [`internal/atlas/README.md`](../atlas/README.md), "How waypoints and patterns are stored".
- **Why a circular buffer:** dropping stale scans from the bottom never moves the rest. `At(i)` is `scans[(bottom+i) % Cap]`.
- **Stale correct scans** stay in `Stack` until the expiry timer fires (the first stale microsecond) or the next accepted scan rebuilds the stack from the live ones (or a selection or completion purges it). Every reader uses `Live`, so they're never seen, even in the moment before `expire` runs. Wrong scans never leave until a purge.
- **The timer lives on the `entry`, not in `State`:** it's not data a client sees, and snapshots stay plain values.
- **The array is 16 long** although only **6** (`game.max_pattern_points`) are used, because Go array sizes must be constants and `game.max_pattern_points` is read at startup. 16 is the most `teams.size` allows, and a pattern can't have more points than members.

## Data model: Postgres
The content tables (`waypoint`, `shape`, `pattern`, `pattern_waypoint`, migration `0003`) belong to the atlas: see [`internal/atlas/README.md`](../atlas/README.md), "Data model: Postgres". The per-team tables below refer to them (`team_scan.waypoint_id` → `waypoint`, `team_pattern.pattern_id` → `pattern`, `team_selection.shape_id` → `shape`).

Per team, written through:

| Table | Migration | Columns | Constraints and what they back |
|---|---|---|---|
| `team_scan` | `0003`, `0006` | `id` BIGINT identity, `team_id`, `waypoint_id`, `user_id`, `scanned_at`, `wrong` | One row per **accepted** scan (rejected scans are never written): the audit trail, and the source of the stack. `wrong` (`0006`) marks a scan that didn't fit the selected shape, so a restart reproduces it as never-stale. `0005` records the table's `COMMENT`, correcting 0003's SQL comment ("every scan, including repeats"). One index, `team_scan_team_id_idx` on `(team_id, id DESC)` (`0004`), serves the stack query and every other query by team; `0005` dropped 0003's redundant `team_scan_team_idx` on `(team_id)`. |
| `team_pattern` | `0003` | `team_id`, `pattern_id`, `completed_at` | PK `(team_id, pattern_id)`: a pattern counts once |
| `team_selection` | `0004` | `team_id`, `shape_id`, `selected_by`, `selected_at`, `after_scan_id` BIGINT | PK `team_id`: one selection per team. `after_scan_id` is the team's latest `team_scan.id` when the shape was selected: the stack is the scans after it |
| `team_cast` | `0007` | `id` BIGINT identity PK, `team_id` VARCHAR(6), `pattern_id` INTEGER, `cast_by` UUID, `cast_at` TIMESTAMPTZ, `already_completed` BOOLEAN, all `NOT NULL` | One row per **cast**, repeats included (unlike `team_pattern`): the cast log, and the source of `LastCast`. `pattern_id` → `pattern` (a cast is of a real pattern); `cast_by` → `userprofile` (a real user); `already_completed` records whether `team_pattern` already had the pattern. `id` orders casts: the highest is the newest. Index `team_cast_team_id_idx` on `(team_id, id DESC)` serves the newest-cast query. No unique constraint: any number of casts per team and pattern |

Every per-team table has `team_id` → `team` with `ON DELETE CASCADE`. `team_cast` is written only inside `insertCompletingScan`'s transaction, so a cast is logged exactly when its scan and its `team_pattern` row are.

**The stack isn't a table.** It's derived: the newest **6** (`game.max_pattern_points`) `team_scan` rows with `id > after_scan_id`. Selecting moves `after_scan_id` to the latest scan (purge); completing or clearing deletes the selection (no selection, no stack). Stale correct rows come back from the query but are filtered by time, exactly as in memory; wrong rows are kept, exactly as in memory. So a restart reproduces the live stack exactly.

## Memory ↔ Postgres
| Postgres | Rebuilt in memory (by `Load`, or `reconcile` for one team) |
|---|---|
| `waypoint`, `shape`, `pattern`, `pattern_waypoint` | Not here: the atlas, rebuilt by `atlas.Load` before this `Load` ([`internal/atlas/README.md`](../atlas/README.md)) |
| `DISTINCT waypoint_id FROM team_scan` | `State.Waypoints` |
| `team_pattern` | `State.Patterns`, and `State.Shapes` from each pattern's shape |
| `team_selection` | `State.Selection` |
| newest `team_cast` row per team (`DISTINCT ON (team_id) … ORDER BY team_id, id DESC`) | `State.LastCast` (`Active: true`; none → zero `Cast`, `last_cast: null`) |
| newest **6** (`game.max_pattern_points`) `team_scan` with `id > after_scan_id`, by `id` | `State.Stack` (pushed oldest first, with the `wrong` flag) |
| (none: built from the atlas and `config.toml`) | `Store.game`, rebuilt by `newGame` at every `Load` |
| (none: memory-only) | `entry.stopExpiry`, the expiry timer: re-armed by `Load` for every team from its reloaded stack, so it fires at the same moment it would have |
| (none: memory-only, from teams) | `entry.members`, the roster cache: refilled from `Teams.ByID` on each team's first publish after a restart. teams reloads rosters from `teammember`, and an `IN_GAME` roster never changes, so the cache always matches |

**Casts before migration `0007` aren't logged**, so a team whose only casts predate it reloads with `last_cast: null` although its `patterns` are non-empty. Every cast since is logged, and the newest row is exactly the `LastCast` memory held, so a restart reproduces it.

**`expire` writes nothing.** It only drops stale correct scans from memory. After a restart, `Load` reads those rows again (they're still after `after_scan_id`), but they're stale by their stored `scanned_at`: `view` leaves them out, and the timer `Load` arms is already due, so `expire` drops them at once. Before and after, every reader sees the same stack.

Why the stacks agree: memory pushes every accepted scan and only drops stale *correct* scans, which are always the oldest. The live correct scans fit one pattern, so there are at most **6** (`game.max_pattern_points`) of them, and they're the newest rows since the selection — exactly what the query returns, after the same time filter. Wrong scans are never dropped, in memory or after a restart.

## The client countdown and latency
The server allows a pattern's scans to span **65 s** (window + grace), measured by **its own clock** from the moment it accepted the bottom scan. Every phone counts down to the same moment on that clock, but it can only estimate the server's clock, and its estimate runs late by one network delay. So the client counts down the **window only**, and the **grace pays for the network**.

**Server time on the phone** (`frontend/src/lib/game/serverClock.ts`, class `ServerClock`). Every event stream opens with `hello {server_time}` (AGENTS.md §9). On each `hello`, the phone sets:
- offset = `Date.parse(server_time)` − localNow, where localNow = `performance.timeOrigin + performance.now()` (monotonic: never jumps when the user or the network changes the wall clock)
- server now = localNow + offset

Every reconnect sends a new `hello`, so the offset is re-measured. Before the first `hello` the offset is 0 (the phone's own clock).

**The rule** (`frontend/src/lib/game/scanGate.ts`, class `ScanGate`):
1. **Deadline** = the stack's **bottom** scan's `scanned_at` + **1 m** (`game.scan_window`, from `scan_window_ms`), read from the latest `team_state` (or a scan's `200`). Later scans don't move it.
2. **No countdown** while the stack is **empty**: a new selection, a completion, a clear, or the server's expiry event dropping the last correct scan.
3. **Stop sending** once server now (`ServerClock.now()`) reaches the deadline: the phone sends no scan at all. The team selects again to start over.
4. **A new bottom sets a new deadline.** When the bottom scan goes stale, the server publishes a `team_state` without it, and the next scan's `scanned_at` + window is the new deadline.

The window comes from `GET /api/game/shapes`; the grace is never shown or counted down. The phone never decides state from time: it only stops sending. The server still publishes every expiry itself.

**The bound.** Let the server accept the bottom scan at server time *T*, so the deadline is *T* + *W* on the server's clock. For one phone:
- *d*: how late that phone's `hello` arrived (the response delay). Its estimate of server time runs *d* behind, never ahead, so it stops at server time ≤ *T* + *W* + *d*.
- *u*: how long that phone's last scan takes to reach the server (request plus processing up to `now()`). The server stamps it before *T* + *W* + *d* + *u*.
- The server accepts it with the bottom still live if the span is at most *W* + *G*.

So **a phone is safe whenever *d* + *u* ≤ *G***: the `hello` delay plus the request delay fits in the grace, **5 s** (`game.scan_grace`). Beyond that, the bottom scan is already stale when the late scan lands: the server drops it and answers `scan_added`, not `pattern_completed` (walkthrough 3's behaviour), and the team rescans that point or selects again.

- **The phone's own clock error doesn't matter.** `hello` cancels it: a phone 9 s fast gets an offset 9 s smaller, and lands on the same server time.
- ***d* is the `hello` delay, not the current one.** If the network was slow when the stream opened and fast later (or the other way round), the error is what `hello` took. The next reconnect re-measures it.
- **A late `team_state` is safe.** A phone that learns of the bottom scan late gets the same deadline, so it just sees less of the countdown.

**Why this design:**
- **The same deadline on every phone.** It's computed from a state timestamp, not from when each phone happened to see the stack, so teammates' countdowns end together.
- **It survives reloads.** A page loaded mid-attempt reads the bottom `scanned_at` and gets the deadline everyone else has.
- **It's immune to phone clock error.** Only the offset from `hello` is used, never the phone's wall clock.

**Worked example**, config as shipped (*W* = **60 s**, *G* = **5 s**), square `1001 1002 1007 1008`, server times after 12:00. A's stream opened earlier: its `hello` was stamped 11:58:00.000 and arrived 0.2 s later, so A's estimate runs 0.2 s behind. D's `hello` (stamped 11:58:30.000) took 1.5 s, so D's runs 1.5 s behind. D's phone clock is also 9 s fast; the offset cancels that.

| Server time | Event | A's estimate | D's estimate |
|---|---|---|---|
| 00:00.000 | Server accepts A's `1001` (bottom), stack `1001/A@00.000`; deadline **01:00.000** for everyone | | |
| 00:00.200 | A's `200` arrives | deadline 01:00.000 | |
| 00:01.500 | D's `team_state` event arrives | | deadline 01:00.000 |
| 00:10 – 00:30 | B and C scan `1002`, `1007`; accepted | running | running |
| 01:00.200 | A's countdown ends | 01:00.000 | 00:58.700 |
| 01:01.400 | D taps scan `1008`, 0.1 s before D's countdown ends | | 00:59.900: open, sent |
| 01:03.400 | The request reaches the server (*u* = 2.0 s) | | |
| | Span 63.4 s ≤ 65 s: **`pattern_completed`**. *d* + *u* = 3.5 s ≤ 5 s | no countdown (empty stack) | no countdown |

Had D's request taken 4.0 s instead (*d* + *u* = 5.5 s > 5 s), it would land at 01:05.400: A's scan is 65.4 s old, stale and dropped first, so the answer is `scan_added` with stack `1002 1007 1008`.

**Tested** end to end in `internal/server/tests/simulation_test.go`, with the window shrunk to 2 s and the grace to 1 s so the tests run in seconds. Each simulated player has its own request and response delays, its own event stream, an offset measured from `hello` (delivered one response delay late), an optional phone clock error (`skew`), and a Go copy of `ScanGate`:

| Test | Shows |
|---|---|
| `TestSimTeamCompletesUnderLatency` | Four players scan the square at once, each with different delays up to 150 ms each way: exactly one `pattern_completed`; every screen ends with an empty stack, the square completed and no countdown |
| `TestSimLastScanAtDeadlineWithinGrace` | The last player, 300 ms each way (0.6 s ≤ 1 s grace), sends 20 ms before their own deadline (on their `hello`-corrected clock, 300 ms behind the server's): `pattern_completed` |
| `TestSimLatencyBeyondGraceLosesBottomScan` | The same with 700 ms each way (1.4 s > 1 s grace): the bottom scan is stale on arrival, so `scan_added` |
| `TestSimGateClosesThenReselectOpens` | After the deadline the client sends nothing; a reselect empties the stack, every player's countdown ends, and the square can be scanned again |
| `TestSimRaceSameWaypoint` | Two players scan the same waypoint at once: one `200`, one `409 waypoint_already_scanned`; every screen converges on a one-scan stack |
| `TestSimStaleScanReachesEveryone` | In real time, after one scan with nobody acting, every player's stream shows an empty stack once the scan is older than window + grace (3 s), and no countdown runs |
| `TestSimSkewedClocksShareTheDeadline` | Six phones with clocks off by −9, +7, 0, +4, −3 and +9 s (20 ms each way) re-measure their offset from `hello`: all compute the identical server-time deadline, and the phone 9 s fast still lands its last scan 50 ms before its deadline: `pattern_completed` |

The countdown and the clock are unit-tested in `frontend/src/lib/game/scanGate.test.ts` (9 tests, `ScanGate` and `ServerClock`; `bun run test` in `frontend/`). Keep `serverClock.ts`, `scanGate.ts` and the Go `simClient` and `scanGate` in `internal/server/tests/simulation_test.go` in step.

## Walkthroughs
Team `482913`, members A–F, 2025 seed (square = `1001 1002 1007 1008` = indexes 0, 1, 6, 7; `square` = shape 2), config.toml as shipped (limit 65 s). Times are `mm:ss` after 12:00. Stacks are written bottom → top as `waypoint/member@time`.

### 1. A square, with every rejection
| Time | Request | Check | Stack after | Postgres after | Response |
|---|---|---|---|---|---|
| 00:00 | F: select `square` | team IN_GAME, shape known | empty | `team_selection(square, F, after_scan_id = latest)` | `200`, `selection.shape = "square"` |
| 00:01 | A: scan `1001` | `{1001}` is inside some square | `1001/A@01` | `team_scan` +1 | `scan_added`, `expires_at 01:06` |
| 00:02 | A: scan `1001` (a retry) | **1001 is already in the stack** | unchanged | unchanged | `409 waypoint_already_scanned` |
| 00:05 | B: scan `1002` | `{1001,1002}` = 3 is inside the square 195 | `1001/A 1002/B@05` | `team_scan` +1 | `scan_added` |
| 00:06 | A: scan `1007` | **A already has a scan in the stack** | unchanged | unchanged | `409 member_already_scanned` |
| 00:07 | C: scan `1001` | **1001 is already in the stack** | unchanged | unchanged | `409 waypoint_already_scanned` |
| 00:10 | C: scan `1007` | set 67 fits the square; 67 is also a **triangle** (shape 4 ≠ 2), so no completion | `… 1007/C@10` | `team_scan` +1 | `scan_added` |
| 00:12 | D: scan `1008` | set 195 = square, shape 2 = selection | **purged**; selection cleared; `LastCast` = square 195 by D @12 | `team_scan` +1, `team_pattern` +1, `team_cast` +1, `team_selection` deleted | `pattern_completed`, `pattern ["1001","1002","1007","1008"]`, `last_cast.cast_by` = D |
| 00:13 | D: scan `1008` (a retry) | no selection | unchanged | unchanged | `409 no_shape_selected` |

### 2. A wrong scan poisons the selection
| Time | Request | Check | Stack after (wrong in *italics*) | Response |
|---|---|---|---|---|
| 00:00 | F: select `square` | | empty | `200` |
| 00:01 | A: `1001` | inside a square | `1001/A@01` | `scan_added` |
| 00:02 | B: `1002` | inside a square with 1001 | `1001/A 1002/B@02` | `scan_added` |
| 00:03 | C: `1003` | no square has 1001, 1002 *and* 1003 | `1001/A 1002/B *1003/C@03*` | `scan_added` (wrong) |
| 00:04 | C: `1007` | **C already has a scan** (the wrong one counts) | unchanged | `409 member_already_scanned` |
| 00:05 | D: `1008` | 1003 poisons the set: no square contains it | `… *1003/C* 1008/D@05` | `scan_added` (also wrong; no completion) |
| 00:06 | F: select `square` | reselect always purges | empty (1003 gone) | `200` |
| 00:07–10 | A,B,C,D: `1001 1002 1007 1008` | exact square | **purged** | `pattern_completed` |

The wrong `1003` never goes stale: without the reselect at 00:06 the team stays stuck until it selects again (or clears).

### 3. One slow member doesn't lose the pattern
| Time | Request | Stack after (live) | Response |
|---|---|---|---|
| 00:00 | F: select `square` | empty | `200` |
| 00:00 | A: `1001` | `1001/A@00` | `scan_added` |
| 00:10 | B: `1002` | `1001/A@00 1002/B@10` | `scan_added` |
| 00:20 | C: `1007` | `… 1007/C@20` | `scan_added` |
| 01:10 | D: `1008` | A's scan is 70 s old: **stale, dropped**. `1002/B@10 1007/C@20 1008/D@70` | `scan_added` (3 points aren't a square) |
| 01:11 | A: `1001` again | not rejected: A's old scan is stale, so 1001 and A are free. `1002/B 1007/C 1008/D 1001/A@71` | `pattern_completed` |

Only A rescanned. Had A's rescan come at 01:16, B's scan (66 s old) would have gone stale too, and B would rescan in turn.

### 4. Reselecting purges
F selects `square`; A, B, C scan `1001 1002 1007`. F selects `square` again: the stack is purged (`after_scan_id` moves past those three rows). D scans `1008`: `scan_added`, stack `1008/D` only. A may scan `1001` at once (their earlier scan is gone).

### 5. Grace boundaries
Bottom scan at 00:00, everything else in order, top scan at:

| Top at | Span | Result |
|---|---|---|
| 00:59 | 59 s | within the window: completes |
| 01:02.5 | 62.5 s | over the window, inside the grace: completes |
| 01:05.000000 | exactly 65 s | completes (the limit is inclusive) |
| 01:05.000001 | 65 s + 1 µs | the bottom scan is stale and dropped first: `scan_added` |

### 6. A scan goes stale with nobody acting
Limit 65 s, so a correct scan stamped at *t* is live through *t* + 65 s exactly and stale from *t* + 65 s + 1 µs.

| Time | What happens | Timer after | Stack (memory) | Postgres | Event to every member |
|---|---|---|---|---|---|
| 00:00 | F: select `square` | none (no correct scans) | empty | `team_selection` upserted | `team_state`, stack `[]` |
| 00:10 | A: `1001` | armed for **01:15.000001** | `1001/A@10` | `team_scan` +1 | `team_state`, stack `[1001]`, `expires_at 01:15` |
| 00:30 | B: `1002` | cancelled and re-armed for 01:15.000001 (A's is still the oldest) | `1001/A@10 1002/B@30` | `team_scan` +1 | `team_state`, stack `[1001, 1002]` |
| 01:15.000000 | (nothing: A's scan is exactly 65 s old, still live) | | | | none |
| 01:15.000001 | **Timer fires**: `expire` finds `1001/A` stale, drops it | armed for **01:35.000001** (B's) | `1002/B@30` | unchanged (memory-only) | `team_state`, stack `[1002]` |
| 01:20 | A: `1001` again | re-armed for 01:35.000001 (B's is now the oldest) | `1002/B@30 1001/A@80` | `team_scan` +1 | `team_state`, stack `[1002, 1001]` |
| 01:35.000001 | Timer fires: drops `1002/B` | armed for **02:25.000001** (A's rescan) | `1001/A@80` | unchanged | `team_state`, stack `[1001]` |
| 02:25.000001 | Timer fires: drops `1001/A` | **none** (no correct scans left) | empty; selection still `square` | unchanged | `team_state`, stack `[]` |

Each `team_state` moves every phone's deadline to the new bottom's `scanned_at` + 60 s: 01:10 (A's `1001@10`), then 01:30 (B's, after the first expiry), then 02:20 (A's rescan); the empty stack at 02:25.000001 ends the countdown, and A or B can scan again under the same selection. Had the backend restarted at 01:50, `Load` would rebuild `1001/A@10 1002/B@30 1001/A@80` from `team_scan` (every row after `after_scan_id`, newest 6; `expire` never deletes rows). The first two are stale (their expiries, 01:15.000001 and 01:35.000001, are past), so `view` already shows `[1001]`, and the timer `Load` arms is due at once: `expire` drops both, leaving the same `1001/A@80` stack as before the restart. Its event goes to nobody (no streams are open before the backend serves); each member gets the state as their stream reconnects.

A wrong scan arms nothing: a stack of only wrong scans has no timer at all, and never shrinks until a purge.

### 7. `last_cast` after a cast and a repeat
| Time | Request | `last_cast` after | `patterns` | `team_cast` rows | `team_pattern` rows |
|---|---|---|---|---|---|
| (start) | | `null` | `[]` | 0 | 0 |
| 00:12 | D completes square `1001 1002 1007 1008` (walkthrough 1) | `{"pattern":["1001","1002","1007","1008"],"shape":"square","cast_by":D,"cast_at":"…12:00:12Z","already_completed":false}` | `[[1001,1002,1007,1008]]` | 1 | 1 |
| 01:00 | F selects `triangle` | **unchanged** (a selection doesn't clear it) | unchanged | 1 | 1 |
| 02:00 | F selects `square` again | unchanged | unchanged | 1 | 1 |
| 02:01–02:04 | A, B, C, D scan the same square; D's scan completes it | `{…,"cast_by":D,"cast_at":"…12:02:04Z","already_completed":true}` | unchanged (counted once) | 2 | 1 (`ON CONFLICT DO NOTHING`) |
| (restart) | | the row with the highest `id`: the 02:04 cast, `already_completed: true` | unchanged | 2 | 1 |

`ScanResult.already_completed` and `last_cast.already_completed` agree for the scan that cast.

## The write-through
Every write in `write.go` (`Select`, `ClearSelection`, `Scan`) runs these steps, marked in the code (`PublishTeam` is not a write; see the table):
1. **lock** the team's entry (`lockTeam`)
2. **validate**:
   - first the atlas lookup (shape or waypoint ID) and the team, **before** the lock (an `IN_GAME` team's status and roster never change). This is AGENTS.md §6 variation (a); the code labels it `2. validate, before 1. lock`
   - then the `State`, under the lock (`2. validate (state)`)
3. **build** the next `State` (a copy)
4. **persist**: one `db.go` call (`insertScan`, `insertCompletingScan`, `upsertSelection` or `deleteSelection`), detached from the request and capped at **5 s** (`storage.write_timeout`)
5. **apply**: `entry.cur.Store(&next)`
6. **publish** `team_state` to every member, still holding the lock so a team's events are in order; then `scheduleExpiry` re-arms (or cancels) the team's timer for the new state

| Write | `db.go` call (step 4) |
|---|---|
| `Select` | `upsertSelection` |
| `ClearSelection` | `deleteSelection` |
| `Scan`, no completion (wrong included) | `insertScan` |
| `Scan` that casts | `insertCompletingScan` (`team_scan`, `team_pattern`, `team_cast`, delete `team_selection`, one transaction) |
| `expire` (timer) | none: a **memory-only change** (AGENTS.md §6); see [Memory ↔ Postgres](#memory--postgres) |
| `PublishTeam` (start hook) | none: not a write. Only steps 2, 1 and 6; the state is published as it is |

**On a failed persist:** `reconcile` reloads the team from Postgres (Postgres wins), publishes the result, and returns the error, which becomes a `500`. Nothing from the failed write is applied.

**Rejections and no-ops return before step 4:** every `409`, clearing when there's nothing to clear, and an `expire` that finds nothing stale. Nothing is written and nothing is published. Reselecting is **not** a no-op: it always purges, writes and publishes.

## API
All routes need `Authorization: Bearer <token>` (`401` without). Bodies over **1 MiB** (`server.max_body_bytes`) get `400 invalid_body`.

| Method | Path | Body | Response payload |
|---|---|---|---|
| GET | `/api/game/shapes` | none | `Game` |
| GET | `/api/teams/me/state` | none | `View` |
| PUT | `/api/teams/me/state/selection` | `{"shape": "square"}` | `View` |
| DELETE | `/api/teams/me/state/selection` | none | `View` |
| POST | `/api/teams/me/state/scans` | `{"waypoint_id": "1007"}` | `ScanResult` |

**`View`** (also the data of the `team_state` live event):
```json
{
  "team_id": "482913",
  "waypoints": ["1001", "1002"],
  "patterns": [["1003", "1004", "1005", "1006"]],
  "shapes": ["square"],
  "selection": {"shape": "triangle", "selected_by": "6f1c…", "selected_at": "2026-10-17T12:00:00Z"},
  "last_cast": {"pattern": ["1003", "1004", "1005", "1006"], "shape": "square", "cast_by": "9a2b…", "cast_at": "2026-10-17T11:58:40Z", "already_completed": false, "damage": 107},
  "stack": [
    {"waypoint_id": "1001", "user_id": "9a2b…", "scanned_at": "2026-10-17T12:00:01Z", "expires_at": "2026-10-17T12:01:06Z"},
    {"waypoint_id": "1002", "user_id": "c3d4…", "scanned_at": "2026-10-17T12:00:05Z", "expires_at": "2026-10-17T12:01:10Z"}
  ]
}
```
- `selection` is `null` when nothing is selected.
- `last_cast` is the team's newest cast, or `null` if it has none (or cast only before migration `0007`). `pattern` is its sorted waypoint IDs, `shape` its shape ID, `cast_by` the member whose scan completed it, `cast_at` that scan's time, `already_completed` true if the team had cast this pattern before (progress didn't grow), and `damage` is what the cast spell deals. It stays until the next cast: selecting or clearing doesn't remove it.
- `stack` is the live scans, bottom (oldest) first; `[]` when empty. Stale correct scans are left out; wrong scans are included (with no flag telling them apart).
- `expires_at` is `scanned_at` + 65 s (window + grace): when a correct scan goes stale on the server. One microsecond later the server publishes a `team_state` without it. The app's countdown doesn't use it (it counts to the bottom scan's `scanned_at` + 60 s, see [The client countdown and latency](#the-client-countdown-and-latency)); it's for display.

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
  "scan_window_ms": 60000,
  "scan_grace_ms": 5000,
  "max_pattern_points": 6
}
```
- `shapes`: every selectable shape (the client's "types"), in index order (by ID). `id` is what `PUT …/selection` takes; `name` is for display; `patterns` is how many valid patterns the shape has; `points` is the distinct sizes of those patterns, ascending (a shape mixing 3- and 4-point patterns shows `[3, 4]`); `damage` is the minimum and maximum damage across the shape's patterns.
- `scan_window_ms`: **1 m** (`game.scan_window`) in milliseconds: the client's countdown.
- `scan_grace_ms`: **5 s** (`game.scan_grace`) in milliseconds: server-side tolerance only; clients don't add it.
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
- `scan_added`: the scan was pushed; no pattern yet. This includes **wrong** scans, which are indistinguishable from correct ones in the response.
- `pattern_completed`: `pattern` is set; `already_completed` is true if the team had completed this pattern before. The stack and selection are purged.

**Errors** use the envelope, e.g. `409 {"success": false, "code": "member_already_scanned", "message": "you already scanned a waypoint; a teammate must scan the next one"}`.

**Live events:** `team_state` with the `View`, sent:
- to every member after every change: select, clear, accepted scan, cast, and `reconcile` (never after a rejection or no-op)
- to every member when the expiry timer drops a stale correct scan, with nobody acting
- to every member right after the `IN_GAME` `team` event when the leader starts (`PublishTeam`, through teams' `Rules.OnStart`): the empty state
- right after `team` when a member connects to `GET /api/events`

Every trigger, the ordering guarantees and the wire format: [`internal/server/README.md`, "The event stream"](../server/README.md#the-event-stream).

## Rules and cases

### Loading at startup
The backend refuses to start if any of these hold:

| Case | Error |
|---|---|
| `config.toml` is invalid (see `internal/platform/config`), e.g. a missing key, or `max_pattern_points` > `teams.size` | from `config.Load`, before anything loads |
| The content is invalid (bad waypoint IDs, too many items, unknown, empty, oversized, duplicate or nested patterns) | from `atlas.Load`, before this `Load` runs: every case, in order, is in [`internal/atlas/README.md`](../atlas/README.md), "Loading at startup" |
| Reading the per-team tables fails | `load team state: …` |
| Stored visited waypoints name a waypoint the atlas doesn't have | `team 482913 state: unknown waypoint "1012"` |
| A `team_pattern` key the atlas doesn't have | `team 482913 state: unknown pattern 400` |
| A selection's shape the atlas doesn't have | `team 482913 state: selection: unknown shape "hexagon"` |
| A stack scan's waypoint the atlas doesn't have | `team 482913 state: stack scan: unknown waypoint "1012"` |
| The newest `team_cast` row's pattern the atlas doesn't have | `team 482913 state: last cast: unknown pattern 400` |

### Select: `PUT /api/teams/me/state/selection`
Checked in this order:

| Case | Result |
|---|---|
| Body not `{"shape": string}`, unknown fields, empty, or over 1 MiB | `400 invalid_body` |
| Shape unknown | `404 unknown_shape` |
| Caller not in a team | `404 not_in_team` |
| Team not `IN_GAME` | `409 not_in_game` |
| Any member selects (not just the leader) | Allowed |
| Same shape already selected | `200`: **stack purged**, selection re-stamped with the caller and now; written; event |
| A different shape, or nothing selected | `200`: selection set; stack purged; written; event |
| Shape already completed before | Allowed; completing another of its patterns adds the pattern |

### Clear: `DELETE /api/teams/me/state/selection`
| Case | Result |
|---|---|
| Caller not in a team | `404 not_in_team` |
| Team not `IN_GAME` | `409 not_in_game` |
| Nothing selected | `200`, no-op |
| Otherwise | `200`: selection and stack removed; visited waypoints and completions kept; event |

### Scan: `POST /api/teams/me/state/scans`
Checked in this order. "In the stack" means live: a correct scan at most 65 s old, or a wrong scan (which never goes stale). Every rejection changes nothing, isn't logged and sends no event.

| Case | Result |
|---|---|
| Body not `{"waypoint_id": string}`, empty, or over 1 MiB | `400 invalid_body` |
| Waypoint unknown | `404 unknown_waypoint` |
| Caller not in a team | `404 not_in_team` |
| Team not `IN_GAME` | `409 not_in_game` |
| No shape selected (including right after a completion) | `409 no_shape_selected` |
| Waypoint already in the stack, by anyone (a retry included, a wrong scan included) | `409 waypoint_already_scanned` |
| Caller already has a scan in the stack, at another waypoint (wrong scans included) | `409 member_already_scanned` |
| The waypoint, with the correct scans in the stack, isn't part of any pattern of the selected shape | Not rejected: the scan is pushed, marked **wrong**, and poisons the selection (no later scan can complete it) |
| A wrong scan, however old it is | Never goes stale; it stays until the selection is purged |
| An earlier *correct* scan of this waypoint, or by this member, is **stale** | Allowed: it no longer counts |
| Stale correct scans at the bottom | Dropped, then the scan is pushed |
| Stack plus this scan is exactly a pattern of the **selected** shape | `pattern_completed`: pattern and shape marked; **stack and selection purged**; `last_cast` set (`already_completed: false`); `team_cast` +1 |
| ... that pattern was already completed | `pattern_completed` with `already_completed: true`; progress unchanged; `last_cast` replaced (`already_completed: true`); `team_cast` +1, `team_pattern` unchanged |
| Stack plus this scan is a pattern of **another** shape | Not completed → `scan_added` |
| Otherwise | `scan_added` |
| 1-point pattern | Completes on its only scan |
| 6-point pattern | Completes on the sixth scan |
| Span exactly 65 s | Completes. 65 s + 1 µs: the bottom is stale and dropped |
| Any accepted scan (wrong included) | Logged in `team_scan` (with `wrong`); waypoint added to visited `waypoints` |
| Postgres write fails | `500`; state reloaded from Postgres; nothing applied |

### Expiry: the timer, no request
| Case | Result |
|---|---|
| A correct scan exactly 65 s old | Still live; nothing happens |
| The oldest correct scan reaches 65 s + 1 µs | Timer fires: every stale correct scan is dropped from memory; `team_state` to every member; timer re-armed for the next oldest correct scan |
| Two correct scans stale at the same microsecond | Both dropped by one `expire`; one event |
| A write purged or rebuilt the stack before the timer ran (`stop` came too late) | `expire` finds nothing stale: no change, no event; re-armed for the current stack |
| Stack empty, just cast, purged, or only wrong scans | No timer pending |
| Wrong scans in the stack | Never dropped, never timed |
| Selection | Unchanged by expiry: the team keeps its shape with an emptier stack |
| Postgres | Untouched (memory-only) |
| Backend down when a scan went stale | `Load` arms a timer that is already due: dropped and published at once |

### Start: `PublishTeam`, no request (teams' `Rules.OnStart`)
Checked in this order:

| Case | Result |
|---|---|
| No team with that ID | Nothing published |
| Team not `IN_GAME` (lobby) | Nothing published |
| Team `IN_GAME` (always, when called by teams' `Start`) | `team_state` with the current state to every member: just after a start, the empty state. Memory is unchanged except that an empty entry is created if the team had none (reads already report that same empty state); Postgres untouched |
| Called again, or after a write | The current state again: a full snapshot, so a repeat is harmless (`TestPublishTeam`) |

### Last cast
| Case | Result |
|---|---|
| Team never cast | `last_cast: null` |
| A cast | `last_cast` = that pattern, its shape, the completing member, the scan's time, `already_completed` |
| A repeat cast of the same pattern | `last_cast` replaced, `already_completed: true`; logged in `team_cast` again |
| Select, reselect, clear, expiry | `last_cast` unchanged |
| Restart | The newest `team_cast` row (highest `id`) |
| Casts only before migration `0007` | `last_cast: null` after a restart |

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
| Team in the lobby or with no activity | `200`, empty lists, `selection` null |
| All stack scans stale | `stack: []` (wrong scans never go stale, so a wrong scan keeps the stack non-empty). Normally the expiry timer has already dropped them; `view` filters by time anyway |

## Concurrency and locking
- **Writes to one team are serialised** on its `entry.mu`. Different teams run in parallel.
- **Two members scan the same waypoint at once:** exactly one is pushed; the other gets `waypoint_already_scanned`.
- **One member scans two waypoints at once:** exactly one is pushed; the other gets `member_already_scanned`.
- **A phone resends one scan many times at once:** exactly one is pushed and logged; the rest get `waypoint_already_scanned`.
- **A scan and a selection race:** whichever takes the lock first wins. If the selection comes first, the stack is purged and the scan is the first of the new stack.
- **Lock order:** `entry.mu`, then `Store.mu` (only to create an entry). `Store.mu` is never held during I/O.
- **The start hook.** `PublishTeam` runs inside teams' `Start`, which holds the team's teams entry lock; it takes this team's `entry.mu`. So the lock order across the two use cases is teams entry → teamstate entry. teamstate never takes a teams entry lock (`ByID` and `ForUser` take only teams' index lock, briefly), so this can't deadlock. Holding teams' lock is what makes the start's `team` arrive before its `team_state`. A write that slips in between teams' apply and the hook (a member who scans before seeing the start) publishes its own `team_state` first; the hook then publishes the same, current state again.
- **The team itself is read** through `Teams` without the team's lock. That's safe because writes need `IN_GAME`, after which a team's status and roster are frozen.
- **One timer per team, at most.** `scheduleExpiry` stops the pending timer before arming a new one, and only runs with `entry.mu` held, so two can't be armed at once. `stopExpiry` is guarded by `entry.mu`.
- **Timers take the entry lock.** `expire` runs in the timer's goroutine and locks the entry like a write, so it's serialised with `Select`, `ClearSelection` and `Scan` and its event is ordered with theirs. A timer that fires while a write holds the lock waits; if the write rebuilt the stack, `stop` may have come too late, so `expire` re-checks `Live` under the lock and does nothing if nothing is stale.
- **`Load` arms timers only after the whole map is built.** A timer for a stack that went stale during downtime is due at once, and its callback goes through `lockTeam`, which reads `Store.states`; arming during the build would race the map writes.
- **Why the fake clock never fires inside `AfterFunc`.** The store calls `AfterFunc` with `entry.mu` held, and the callback (`expire`) takes `entry.mu`. A fake that ran due callbacks synchronously from `AfterFunc` would deadlock on that lock. The real `time.AfterFunc` always runs `f` in its own goroutine; the fake runs due timers only from the test's `at`, outside any store lock.
- **The memory checks are the only enforcement** of the stack rules: they depend on time, which a unique constraint can't express. They all run under `entry.mu`, and Postgres has one writer (the instance lock in `platform/postgres`).

## Failure and recovery
| Case | Result |
|---|---|
| Postgres times out or drops during a write | `500`. `reconcile` re-reads the team (the write may or may not have committed) and memory matches Postgres |
| Memory and Postgres disagree (rows edited by hand) | The next write that notices (e.g. `deleteSelection` finding no row) fails, reconciles, and members get the corrected state |
| Postgres unreachable | Reads keep working from memory; writes fail |
| Backend restarts | Progress, selections, live stacks and `last_cast` reload exactly; scans keep their original times, so they go stale when they would have. `Load` re-arms every team's expiry timer; a stack that went stale while the backend was down is dropped at once (memory only), before any stream is open |
| `expire` runs while Postgres is down | Unaffected: it touches only memory |
| A failed write's `reconcile` | Re-arms the timer for the reloaded stack |
| `config.toml` changes between restarts | A longer window can revive scans that were stale; a smaller capacity loads fewer scans. Both apply consistently from then on |
| Content edited while running | Not seen until restart (the atlas loads once). Don't: if the new content drops a waypoint, pattern or shape a team's stored state refers to, the next start refuses. See [`internal/atlas/README.md`](../atlas/README.md) |

## Limits and how to change them
| Limit | Where | To change |
|---|---|---|
| Scan window 1 m | `config.toml` `game.scan_window` | Edit and restart |
| Grace 5 s | `config.toml` `game.scan_grace` (0 allowed, but the key must be present) | Edit and restart |
| 6 points per pattern / stack capacity | `config.toml` `game.max_pattern_points` (≤ `teams.size`) | Edit and restart. Content with bigger patterns is refused at startup (by `atlas.Load`) |
| 16 slots in a `Stack` | `config.MaxTeamSize` | Fixed array size; raise with the team-size cap |
| Expiry timers: at most one per team, only while its stack holds a correct scan | `scheduleExpiry` | Not configurable. A `time.AfterFunc` timer is a runtime timer, not a goroutine, until it fires; 10,000 teams mean at most 10,000 timers |
| 64 waypoints, 64 shapes, 2048 patterns | the atlas's set types (`atlas.MaxWaypoints`, `atlas.MaxShapes`, `atlas.MaxPatterns`) | See [`internal/atlas/README.md`](../atlas/README.md), "Limits and how to change them". `State` holds one of each set, so a wider set makes every snapshot bigger |

## Not decided yet
- **⚠ QR verification.** `POST …/scans` accepts the raw 4-digit waypoint ID, so a player can claim a scan without being there. This must be replaced before the event (rotating or signed QR codes, verified server-side).
- **Whether the leader alone should select.** Currently any member can.

## Tests
- The content lookups teamstate relies on (`PatternFor`, `FitsShape`, `SetOf`, validation, the seed loading, `BenchmarkLookups`) are tested in the atlas: [`internal/atlas/README.md`](../atlas/README.md), "Tests".
- `tests/stack_test.go` (no database):

  | Test | Covers |
  |---|---|
  | `TestStack` | bottom-to-top order, overflow dropping the bottom, `Push` leaving the original alone, `Live` at exactly the limit, everything stale an hour later, and a wrong scan staying live however old it is |

- `tests/store_test.go` (real Postgres, schema `teamstate_test`, the 2025 seed loaded through `atlas.Load`, config.toml's rules, fake clock). The fake `clock` implements `teamstate.Clock`: `Now` returns the fake time; `AfterFunc` only records the timer; `at(d)` sets the time to `start` + `d` and then runs every timer now due, **earliest first**, including timers those runs arm, and never from inside `AfterFunc` (see [Concurrency and locking](#concurrency-and-locking)); `pending()` counts timers neither fired nor stopped. Every helper (`scanAt`, `selectAt`) moves the clock with `at`, so due expiries fire before each request, as in real time. Times are written relative to the window (`tick`, a 60th of `game.scan_window`) or to the limit (`rules.Limit()`), so each scenario means the same under any config. The tests use up to 6 distinct members (the 6-point pattern needs all of them), so they need `teams.size` ≥ 6 (`minTestTeam`); with a smaller size in `config.toml` they **skip** rather than fail:

  | Test | Covers |
  |---|---|
  | `TestWritesRequireGame` | input validation and lobby refusals for every write |
  | `TestScanNeedsSelection` | scanning before selecting changes nothing |
  | `TestSquareNeedsDistinctMembers` | walkthrough 1: member and waypoint rejections change nothing, the triangle that doesn't count, completion, `expires_at`, no selection after |
  | `TestEverySelectionPurges` | walkthrough 4; reselect and select-another both purge |
  | `TestRescanIsRejected` | the same member's retry and another member's scan of a stacked waypoint: rejected, not logged, no event |
  | `TestConcurrentRescansAddOneScan` | 8 simultaneous copies → one pushed, seven `waypoint_already_scanned` |
  | `TestLateRepeatDoesNotRejectThePattern` | walkthrough 3 |
  | `TestGraceTolerance` | walkthrough 5: inside the window, inside the grace, exactly the limit, 1 µs over |
  | `TestStaleScanFreesItsWaypointAndMember` | after going stale, another member can scan the waypoint and the member can scan again |
  | `TestWrongScanLocksMember` | a waypoint in no square is pushed and marked wrong; the member can't scan again, and the waypoint slot is taken |
  | `TestWrongScanPoisonsSelection` | a wrong scan blocks completion until a reselect purges it |
  | `TestWrongScanNeverAgesOut` | a wrong scan stays in the stack past the limit while a correct one goes stale; a restart reproduces its `wrong` flag |
  | `TestCompletionPurges` | stack and selection purged; rescans need a new selection |
  | `TestOutOfOrderScanning` | a square completes when scanned in reverse order (a pattern is an unordered set) |
  | `TestSinglePointPattern` | a 1-point pattern |
  | `TestSixPointPatternAtCapacity` | a 6-point pattern completes on the sixth scan |
  | `TestCompletingAgain` | `already_completed`, progress counted once |
  | `TestClearSelection` | clear with and without a selection; waypoints kept |
  | `TestRestartRestoresEverything` | restart reproduces the JSON view exactly; rescans still rejected |
  | `TestConcurrentScans` | both races |
  | `TestFailedWriteReconciles` | reload after a hand-edited database |
  | `TestReadsDoNotTouchDatabase` | reads with the database closed; a failed write leaves memory alone |
  | `TestStateIsPublished` | live events reach teammates |
  | `TestPublishTeam` | `PublishTeam` sends the empty state to every member of a started team; after a selection it sends the current state (square selected) without changing it; nothing for an unknown team or a lobby team |

- `tests/publish_bench_test.go`: **`BenchmarkTeamStatePublish`**, parallel `PublishTeam` calls across 1,666 `IN_GAME` teams (10,000 players at `teams.size` 6; an in-memory `Teams`, so no team rows), every member with an open subscription. It measures the lock, encoding once and queueing for each member: about **1.0 µs** a publish on a 10-core laptop, down from 4.4 µs before the sharded hub and roster cache (`internal/platform/README.md`, "Performance").
  | `TestStaleScanIsPublished` | walkthrough 6: at exactly the limit nothing is sent; at limit + 1 µs the stale scan is dropped and `team_state` published, memory agrees, one timer pending for the next scan; after the second expiry an empty stack and no timer pending; the member can scan again under the same selection |
  | `TestExpiryTimerFollowsWrites` | one timer after two scans; none after a purge; none for a stack of only a wrong scan; none after a cast |
  | `TestLastCast` | walkthrough 7: `last_cast` null before any cast; set (pattern, shape, `cast_by`, `cast_at`) after a cast; `already_completed` on the repeat; kept by a new selection; two `team_cast` rows; a restart reloads the same `last_cast` (JSON equal) |
- `internal/server/tests/teamstate_test.go`: `TestSelectAndScanRoutes`, every route and error code over HTTP, including `waypoint_already_scanned`, `member_already_scanned`, a wrong scan being accepted and locking the member, and the body size limit; `TestGameShapes`, `GET /api/game/shapes` without a token (`401 missing_token`) and with one: the seed's 5 shapes, `square` with 18 patterns of 4 points, and the window, grace and capacity from `config.toml`.
- `internal/server/tests/simulation_test.go`: the countdown under latency and clock error, all seven `TestSim…` tests (table in [The client countdown and latency](#the-client-countdown-and-latency)), including `TestSimStaleScanReachesEveryone` (the expiry reaches every stream in real time) and `TestSimSkewedClocksShareTheDeadline` (phones up to ±9 s off share one deadline).
- `internal/server/tests/events_test.go`: `team_state` on connect; `TestStartPublishesTeamState`: when the leader starts, every open stream gets `team` then the empty `team_state`, in that order.
