# teamstate

What each team does in the game: the shape it has selected, its stack of scans toward a pattern of that shape, the waypoints it has visited, and the patterns and shapes it has completed.

Every number below that comes from `config.toml` is shown with its current value and its key, e.g. **1 m** (`game.scan_window`). Change the file, not the code.

## Contents
1. [Concepts](#concepts)
2. [The game flow](#the-game-flow)
3. [How each file works](#how-each-file-works)
4. [Data model: memory](#data-model-memory)
5. [Data model: Postgres](#data-model-postgres)
6. [Memory ↔ Postgres](#memory--postgres)
7. [How waypoints and patterns are stored](#how-waypoints-and-patterns-are-stored)
8. [The client countdown and latency](#the-client-countdown-and-latency)
9. [Walkthroughs](#walkthroughs)
10. [The write-through](#the-write-through)
11. [API](#api)
12. [Rules and cases](#rules-and-cases)
13. [Concurrency and locking](#concurrency-and-locking)
14. [Failure and recovery](#failure-and-recovery)
15. [Limits and how to change them](#limits-and-how-to-change-them)
16. [Not decided yet](#not-decided-yet)
17. [Tests](#tests)

## Concepts
| Term | Meaning |
|---|---|
| **Waypoint** | A physical location with a QR code. Identified by a **4-digit ID** (`"1001"`; leading zeros allowed). |
| **Pattern** | A set of **1 to 6** (`game.max_pattern_points`) waypoints that forms a shape on the map. Identified **by that set**, e.g. `["1001","1002","1007","1008"]`, in any order. Its database `id` is only an internal key. |
| **Shape** | What a pattern looks like: `square`, `triangle`, `collinear`, `tree`, `lightning` in the 2025 seed. A shape has many patterns. Completing any one completes the shape. |
| **Selection** | The shape a team is currently attempting. At most one per team. |
| **Stack** | The team's accepted scans since its selection, oldest at the **bottom**, newest on **top**. It only ever holds **one scan per waypoint and one per member**, so at most **6** (`game.max_pattern_points`). |
| **Limit** | **65 s**: the scan window **1 m** (`game.scan_window`) plus the grace **5 s** (`game.scan_grace`). The most a pattern's scans may span, bottom to top, and the age at which a *correct* scan goes stale. |
| **Live / stale scan** | A *correct* scan in the stack is **live** while it is at most the limit old (`now − scanned_at ≤ 65 s`), and **stale** after. Stale correct scans don't count and are dropped from the bottom at the next accepted scan. |
| **Wrong scan** | A scan whose waypoint, with the correct scans already in the stack, isn't part of any pattern of the selected shape. It's still pushed (never rejected), occupies its member's one scan and its waypoint slot, poisons the selection (it can no longer complete), and **never goes stale**: it stays until the selection is purged. The client is never told which scans are wrong. |
| **Visited waypoints** | Every waypoint the team has ever had a scan accepted at, under any selection. |
| **Countdown** | The client's timer: **1 m** (`game.scan_window`) from the moment that phone first sees a non-empty stack. After it the phone sends no scans. The grace is never shown; it absorbs network delay (see [The client countdown and latency](#the-client-countdown-and-latency)). |
| **Game rules** | What `GET /api/game/shapes` returns: the selectable shapes (the client's "types") with their pattern counts and sizes, plus the window, grace and stack capacity in milliseconds and points. Static while the backend runs. |

## The game flow
1. **Select.** Any member selects a shape: `PUT /api/teams/me/state/selection {"shape":"square"}`. **Every selection purges the stack**, reselecting the same shape included.
2. **Scan.** Members scan waypoints: `POST /api/teams/me/state/scans {"waypoint_id":"1001"}`. Checked against the stack's live scans, a scan is **rejected with an error and not pushed** if:
   - its **waypoint is already in the stack**, whoever scanned it, the same member included: `409 waypoint_already_scanned`
   - its **member already has a scan in the stack**: `409 member_already_scanned`

   Otherwise stale correct scans are dropped from the bottom and the scan is pushed on top. A scan whose waypoint doesn't fit the selected shape is pushed anyway, marked **wrong** (see [Wrong scan](#concepts)).
3. **Complete.** The moment the stack is **exactly** one of the selected shape's patterns, the pattern and its shape are marked complete, and **the stack and the selection are purged**. The team selects again for its next attempt. A wrong scan poisons the set, so a selection with one can never complete.
4. **Stale correct scans drop on their own.** A correct scan more than 65 s old stops counting, which frees its waypoint and its member: only that point is rescanned, the rest of the pattern isn't lost. Wrong scans never drop. Because every live correct scan is at most 65 s old and the newest is now, the correct stack always spans at most the limit, bottom to top.
5. **Give up.** `DELETE /api/teams/me/state/selection` clears the selection and the stack.

**"Part of a pattern" narrows as the team scans.** The team selects a shape, not a pattern, so the first scan is correct if it's in *any* pattern of the shape. Each later scan must keep the correct stack inside at least one pattern of the shape that contains everything already scanned. A scan that doesn't is wrong and is final: reselect to purge it (it never goes stale on its own).

**Why the shape is selected first.** With the 2025 data any three waypoints form a triangle or a collinear pattern, so a team scanning a square passes through a "triangle" after its third scan. Only the selected shape can complete, which is what makes 4-waypoint shapes possible.

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
  - `State`: the in-memory per-team snapshot, made of `Selection`, `Stack` and `StackScan` (see [Data model: memory](#data-model-memory)).
  - `View`, `SelectionView`, `StackScanView`: the JSON shapes.
  - `Game`, `GameShape`: the JSON of `GET /api/game/shapes`.
- **`Stack` methods** (all on values; none modifies the receiver):
  - `newStack(cap)` is an empty stack.
  - `Len()` counts scans, stale ones included.
  - `At(i)` is the i-th from the bottom.
  - `Push(sc)` returns the stack with `sc` on top. Scan never pushes onto a full stack (its scans always fit one pattern); if it did, the bottom would be dropped.
  - `Live(now, limit)` returns the correct scans at most `limit` old plus every wrong scan, bottom to top.
- **`newGame(content, rules)`** builds the `Game` once, in `Load`: for each shape in index order, its ID and name, how many patterns it has (`len(shapePatterns[i])`), and the distinct sizes of those patterns, ascending; plus `ScanWindowMS`, `ScanGraceMS` (`Duration.Milliseconds()`, so sub-millisecond parts are dropped) and `MaxPatternPoints` from `Rules`.
- **Reads**, all from memory:
  - `Game()`: the prebuilt `Game`; no lock, it never changes
  - `Content()`
  - `ForTeam(teamID)` → `State`
  - `ForUser(userID)` → `View`
  - `Snapshot(userID)`: the live event for a new stream
- **`view`** converts indexes and bits to IDs, times to UTC, and leaves stale correct scans out of `stack` (wrong scans are included, with no flag distinguishing them).

### `write.go`: writes, each written through
- **`Select`**, **`ClearSelection`** and **`Scan`** each follow the [write-through](#the-write-through) steps, numbered in the code.
- **`Scan`'s checks**, in order, over the live scans: same waypoint → `ErrWaypointAlreadyScanned`; same member → `ErrMemberAlreadyScanned`; then `Content.FitsShape(shape, stack ∪ {w})` decides whether the scan is **wrong** (still pushed, marked `Wrong`). Completion is `Content.PatternFor(stack ∪ {w})` (one map lookup) being a pattern of the selected shape; a wrong scan can't complete because it's in the set.
- **`inGameTeam`** fetches the caller's team and requires `IN_GAME`.
- **Declared here:** the errors, the `Outcome…` constants, and `ScanResult`.

### `db.go`: SQL
- **`selectContent`** reads waypoints, shapes, and patterns with their waypoint lists (`array_agg`).
- **`selectTeams(ctx, teamID, stackSize)`** reads every team's stored state, or one team's (`teamID != ""`), into `teamRows`: distinct visited waypoints, completed patterns, selection, and the stack. Used by `Load` and `reconcile`. The stack query is a `JOIN LATERAL`: for each selection, the team's newest `stackSize` `team_scan` rows with `id > after_scan_id`, returned oldest first, with the `wrong` flag so wrong scans rebuild as never-stale.
- **`insertScan`**: one `team_scan` row, with the `wrong` flag (a scan that doesn't complete anything).
- **`insertCompletingScan`**: one transaction: `team_scan` row, `team_pattern` row (`ON CONFLICT DO NOTHING`), delete `team_selection`.
- **`upsertSelection`**: upserts `team_selection` with `after_scan_id` = the team's latest `team_scan.id` (0 if none). That's the purge: earlier scans are no longer after it.
- **`deleteSelection`**: deletes `team_selection`, and fails unless exactly one row went (memory had a selection, so Postgres must too).

### `content.go`: static content and bitsets
- **`newContent(rows, maxPoints)`** validates the content rows (see [Loading at startup](#loading-at-startup)) and builds every lookup table (see [How waypoints and patterns are stored](#how-waypoints-and-patterns-are-stored)).
- **`checkNesting`** rejects a pattern contained in a larger pattern of the same shape: scanning the larger one, the stack would equal the smaller one first and complete it early.
- **Lookups:**
  - `WaypointIndex`, `ShapeIndex`, `SetOf`
  - `PatternFor(set)`: exact match
  - `FitsShape(shape, set)`: is `set` inside some pattern of `shape`? One AND-compare per pattern of the shape, stopping at the first match (at most 156, the seed's triangles). The empty set fits nothing. Used to label a scan wrong, not to reject it. See [Does a set still fit the shape?](#does-a-set-still-fit-the-shape)
  - `WaypointIDs(set)`: a set back to sorted IDs (views, `ScanResult.Pattern`)
- **Bitsets:** `WaypointSet`, `ShapeSet` and `PatternSet`, with their `Has`, `With`, `Contains`, `Len` and `Indexes` methods.

### `store.go`: the in-memory layout
- **`Rules`** are the settings from `config.toml`: `ScanWindow`, `ScanGrace`, `MaxPatternPoints`, `WriteTimeout`. `limit()` is window + grace.
- **`Load`** reads content and every team's stored state, and builds the `Game` (`newGame`). It runs after `teams.Load`, because it reads teams through the `Teams` interface.
- **`stateOf(teamRows)`** rebuilds a `State` from IDs, failing on unknown waypoints, patterns or shapes.
- **`lockTeam`** gets or creates a team's entry and locks it.
- **`persist`** runs a `db.go` write detached from the request, capped at **5 s** (`storage.write_timeout`).
- **`reconcile`** reloads one team's state from Postgres after a failed write and publishes it.
- **`publish`** and **`event`** send `team_state` to every member.
- **`now`** is the clock (UTC, truncated to microseconds, Postgres's precision). Tests swap it for a fake.

## Data model: memory
```
Store
  rules    Rules                     from config.toml
  content  *Content                  static; read with no lock
  game     Game                      built once by newGame; read with no lock
  mu       sync.RWMutex              guards the map below; never held during I/O
  states   map[teamID]*entry         created on a team's first write
  now      func() time.Time          the clock

entry
  mu   sync.Mutex                    writers to this team queue here
  cur  atomic.Pointer[State]         the current snapshot; readers load it lock-free

State                                (never modified once stored)
  Waypoints  WaypointSet  uint64     visited waypoints, bit i = waypoint index i
  Patterns   PatternSet   [32]uint64 completed patterns, bit i = pattern index i
  Shapes     ShapeSet     uint64     completed shapes, bit i = shape index i
  Selection  Selection
    Active   bool                    false → no selection; other fields zero
    Shape    int                     index into Content.Shapes
    By       uuid.UUID               the member who selected
    At       time.Time               when
  Stack      Stack                   scans since the selection

Stack                                a value; Push returns a new one
  scans    [16]StackScan             circular buffer; 16 = config.MaxTeamSize,
                                     only the first Cap slots are used
  bottom   int                       slot of the oldest scan
  n        int                       0…Cap scans held
  Cap      int                       game.max_pattern_points (6)

StackScan
  Waypoint int                       index into Content.Waypoints
  UserID   uuid.UUID                 who scanned
  At       time.Time                 when
  Wrong    bool                      the scan didn't fit the selected shape;
                                     never ages out; not exposed in View
```
- **Size:** a `State` is a fixed 1248 bytes (`Stack` 920, `Selection` 56), so a new snapshot is a plain struct copy with no heap allocation for its contents. 10,000 teams' states are about 12 MB.
- **Why a circular buffer:** dropping stale scans from the bottom never moves the rest. `At(i)` is `scans[(bottom+i) % Cap]`.
- **Stale correct scans** stay in `Stack` until the next accepted scan rebuilds it from the live ones (or a selection or completion purges it). Every reader uses `Live`, so they're never seen. Wrong scans never leave until a purge.
- **The array is 16 long** although only **6** (`game.max_pattern_points`) are used, because Go array sizes must be constants and `game.max_pattern_points` is read at startup. 16 is the most `teams.size` allows, and a pattern can't have more points than members.

## Data model: Postgres
Content (migration `0003`), read once at startup:

| Table | Columns | Constraints and what they back |
|---|---|---|
| `waypoint` | `id` TEXT, `name`, `latitude`, `longitude` | `id ~ '^[0-9]{4}$'`: 4-digit IDs |
| `shape` | `id` TEXT, `name` | `id` lowercase slug |
| `pattern` | `id` INTEGER, `shape_id` | `shape_id` → `shape` |
| `pattern_waypoint` | `pattern_id`, `waypoint_id` | PK on both; FKs to `pattern` and `waypoint` |

Per team, written through:

| Table | Migration | Columns | Constraints and what they back |
|---|---|---|---|
| `team_scan` | `0003`, `0006` | `id` BIGINT identity, `team_id`, `waypoint_id`, `user_id`, `scanned_at`, `wrong` | One row per **accepted** scan (rejected scans are never written): the audit trail, and the source of the stack. `wrong` (`0006`) marks a scan that didn't fit the selected shape, so a restart reproduces it as never-stale. `0005` records the table's `COMMENT`, correcting 0003's SQL comment ("every scan, including repeats"). One index, `team_scan_team_id_idx` on `(team_id, id DESC)` (`0004`), serves the stack query and every other query by team; `0005` dropped 0003's redundant `team_scan_team_idx` on `(team_id)`. |
| `team_pattern` | `0003` | `team_id`, `pattern_id`, `completed_at` | PK `(team_id, pattern_id)`: a pattern counts once |
| `team_selection` | `0004` | `team_id`, `shape_id`, `selected_by`, `selected_at`, `after_scan_id` BIGINT | PK `team_id`: one selection per team. `after_scan_id` is the team's latest `team_scan.id` when the shape was selected: the stack is the scans after it |

Every per-team table has `team_id` → `team` with `ON DELETE CASCADE`.

**The stack isn't a table.** It's derived: the newest **6** (`game.max_pattern_points`) `team_scan` rows with `id > after_scan_id`. Selecting moves `after_scan_id` to the latest scan (purge); completing or clearing deletes the selection (no selection, no stack). Stale correct rows come back from the query but are filtered by time, exactly as in memory; wrong rows are kept, exactly as in memory. So a restart reproduces the live stack exactly.

## Memory ↔ Postgres
| Postgres | Rebuilt in memory (by `Load`, or `reconcile` for one team) |
|---|---|
| `waypoint`, `shape`, `pattern`, `pattern_waypoint` | `Content` and all its lookup tables |
| `DISTINCT waypoint_id FROM team_scan` | `State.Waypoints` |
| `team_pattern` | `State.Patterns`, and `State.Shapes` from each pattern's shape |
| `team_selection` | `State.Selection` |
| newest **6** (`game.max_pattern_points`) `team_scan` with `id > after_scan_id`, by `id` | `State.Stack` (pushed oldest first, with the `wrong` flag) |
| (none: built from `Content` and `config.toml`) | `Store.game`, rebuilt by `newGame` at every `Load` |

Why the stacks agree: memory pushes every accepted scan and only drops stale *correct* scans, which are always the oldest. The live correct scans fit one pattern, so there are at most **6** (`game.max_pattern_points`) of them, and they're the newest rows since the selection — exactly what the query returns, after the same time filter. Wrong scans are never dropped, in memory or after a restart.

## How waypoints and patterns are stored

### Names: Shape and Pattern
The client's word **"type"** is our **Shape** (`square`, `triangle`, …), and its **"shape"** is our **Pattern** (one particular set of waypoints, e.g. `1001 1002 1007 1008`). This README and the code always use Shape and Pattern.

### Indexes
At startup every waypoint, shape and pattern gets a small **index**: its position in a `Content` slice.
- Waypoints and shapes are ordered by ID; patterns by their Postgres key.
- With the 2025 seed: waypoints `1001`→0 … `1011`→10; shapes `collinear`→0, `lightning`→1, `square`→2, `tree`→3, `triangle`→4; patterns 0–358.
- Indexes exist **only in memory** and may change between restarts. Postgres, the API and live events always use IDs.

### Sets are bitmasks
Bit *i* means item *i*:

| Type | Go type | Holds |
|---|---|---|
| `WaypointSet` | `uint64` | up to 64 waypoints |
| `ShapeSet` | `uint64` | up to 64 shapes |
| `PatternSet` | `[32]uint64` | up to 2048 patterns |

Example: the square `1001, 1002, 1007, 1008` is indexes 0, 1, 6, 7, so its `WaypointSet` is `0b11000011` = **195**. Any scan order sets the same bits.

### Static content: `Content` (built once, never written, so no locks)
| Field | Maps | Used for |
|---|---|---|
| `Waypoints`, `Shapes`, `Patterns` | index → item. A `Pattern` is `{dbID, Shape index, Waypoints bitmask}` | turning indexes back into IDs |
| `waypointIndex` | `"1007"` → 6 | validating a scan, `SetOf` |
| `shapeIndex` | `"square"` → 2 | validating a selection |
| `patternBySet` | bitmask → pattern index, e.g. `195` → the square | the completion check, in one map lookup (`PatternFor`; about 2 ns on the `content_test.go` fixture, `BenchmarkScanLookups`) |
| `shapePatterns` | shape index → its patterns | `FitsShape`, `checkNesting`, `newGame` (each shape's pattern count and sizes) |
| `patternByDBID` | Postgres key → pattern index | loading `team_pattern` |

### Bit operations
| Question | Operation |
|---|---|
| Visited 1007? | `Waypoints & (1<<6) != 0` (`Has`) |
| Stack plus this scan | `set | 1<<w` (`With`) |
| Still inside a pattern of the shape? | any `p` of the shape with `p & set == set` (`Contains`, in `FitsShape`) |
| Stack *is* a pattern? | `patternBySet[set]` (exact) |

### Does a set still fit the shape?
Every scan asks `FitsShape(shape, stack ∪ {w})`: is there a pattern of the selected shape that contains every waypoint so far? A `false` doesn't reject the scan — it marks it **wrong** and poisons the selection.

1. The empty set fits nothing (returns `false` at once). A scan's set always has at least the new waypoint, so this only matters to direct callers.
2. For each pattern index `p` in `shapePatterns[shape]`: if `Patterns[p].Waypoints & set == set` (`Contains`), return `true`.
3. None matched: `false`.

The cost is one AND-compare per pattern of the shape, with an early exit. The 2025 seed has:

| Shape | Patterns |
|---|---|
| `collinear` | 9 |
| `lightning` | 77 |
| `square` | 18 |
| `tree` | 99 |
| `triangle` | 156 |

Measured once on the 2025 seed, by hand (the committed `BenchmarkScanLookups` runs on the small test fixture instead): about **1 ns** when a match is found (early), about **9 ns** for a miss (every pattern of the largest shape checked).

**Why not a lookup table.** The alternative is a precomputed map from every `(shape, subset of one of its patterns)` to `true`. On the seed that's **983** entries, and, measured once on the 2025 seed, it took a flat **15–19 ns**: hashing a key costs more than scanning at most 156 `uint64`s. So the loop stays. Revisit only if a shape grows to thousands of patterns.

## The client countdown and latency
The server allows a pattern's scans to span **65 s** (window + grace), measured by **its own clock** from the moment it accepted the bottom scan. A phone can't see that moment: it only learns of the first scan when a response or event reaches it, after some network delay. So the client counts down the **window only**, and the **grace pays for the network**.

**The rule** (`frontend/src/lib/game/scanGate.ts`, class `ScanGate`):
1. **Start** the countdown when this client first sees a **non-empty** stack: its own `200` for the first scan, or a teammate's scan arriving as a `team_state` event. Later scans don't restart it.
2. **Reset** it whenever the client sees an **empty** stack: a new selection, a completion, a clear.
3. **Stop sending** once **1 m** (`game.scan_window`, from `scan_window_ms`) has passed: the phone sends no scan at all. The team selects again to start over.

The window comes from `GET /api/game/shapes`; the grace is never shown or counted down.

**The bound.** Let the server accept the bottom scan at time *T*. For one phone:
- *d*<sub>down</sub>: how long the news of that scan (its `200`, or the `team_state` event) takes to reach the phone. Its countdown starts at *T* + *d*<sub>down</sub> and ends at *T* + *d*<sub>down</sub> + *W*.
- *d*<sub>up</sub>: how long that phone's last scan takes to reach the server (request plus processing up to `now()`).
- The last scan is sent before the countdown ends, so the server stamps it before *T* + *W* + *d*<sub>down</sub> + *d*<sub>up</sub>.
- The server accepts it with the bottom still live if the span is at most *W* + *G*.

So **a phone is safe whenever *d*<sub>down</sub> + *d*<sub>up</sub> ≤ *G***: the response (or event) delay plus the request delay fits in the grace, **5 s** (`game.scan_grace`). Beyond that, the bottom scan is already stale when the late scan lands: the server drops it and answers `scan_added`, not `pattern_completed` (walkthrough 3's behaviour), and the team rescans that point or selects again.

- **Why not count down to `expires_at`.** That needs the phone's clock to agree with the server's. Counting from when the phone itself saw the stack uses only its own monotonic clock (`performance.now()`).
- **A stale bottom drops but the countdown doesn't reset.** The stack is still non-empty, so the countdown keeps running from the first scan seen. The new bottom is later, so the phone is only more conservative.
- **Page load mid-attempt.** `/play` starts the countdown from the bottom scan's `scanned_at` against the phone's wall clock. Clock skew shifts it; the grace absorbs small skew.

**Worked example**, config as shipped (*W* = **60 s**, *G* = **5 s**), square `1001 1002 1007 1008`, times after 12:00:

| Time | Event | A's countdown | D's countdown |
|---|---|---|---|
| 00:00.000 | Server accepts A's `1001` (bottom), stack `1001/A@00.000` | | |
| 00:00.200 | A's `200` arrives (*d*<sub>down</sub> = 0.2 s) | starts; ends 01:00.200 | |
| 00:01.500 | D's `team_state` event arrives over a slow link (*d*<sub>down</sub> = 1.5 s) | | starts; ends 01:01.500 |
| 00:10 – 00:30 | B and C scan `1002`, `1007`; accepted | running | running |
| 01:01.400 | D taps scan `1008`, 0.1 s before D's countdown ends | | still open: sent |
| 01:03.400 | The request reaches the server (*d*<sub>up</sub> = 2.0 s) | | |
| | Span 63.4 s ≤ 65 s: **`pattern_completed`**. *d*<sub>down</sub> + *d*<sub>up</sub> = 3.5 s ≤ 5 s | reset (empty stack) | reset |

Had D's request taken 4.0 s instead (*d*<sub>down</sub> + *d*<sub>up</sub> = 5.5 s > 5 s), it would land at 01:05.400: A's scan is 65.4 s old, stale and dropped first, so the answer is `scan_added` with stack `1002 1007 1008`.

**Tested** end to end in `internal/server/simulation_test.go`, with the window shrunk to 2 s and the grace to 1 s so the tests run in seconds. Each simulated player has its own request and response delays, its own event stream, and a Go copy of `ScanGate`:

| Test | Shows |
|---|---|
| `TestSimTeamCompletesUnderLatency` | Four players scan the square at once, each with different delays up to 150 ms each way: exactly one `pattern_completed`; every screen ends with an empty stack, the square completed and its countdown reset |
| `TestSimLastScanAtDeadlineWithinGrace` | The last player, 300 ms each way (0.6 s ≤ 1 s grace), sends 20 ms before their own countdown ends: `pattern_completed` |
| `TestSimLatencyBeyondGraceLosesBottomScan` | The same with 700 ms each way (1.4 s > 1 s grace): the bottom scan is stale on arrival, so `scan_added` |
| `TestSimGateClosesThenReselectOpens` | After a countdown runs out the client sends nothing; a reselect resets every player's countdown and the square can be scanned again |
| `TestSimRaceSameWaypoint` | Two players scan the same waypoint at once: one `200`, one `409 waypoint_already_scanned`; every screen converges on a one-scan stack |

The countdown itself is unit-tested in `frontend/src/lib/game/scanGate.test.ts` (`bun run test` in `frontend/`). Keep `scanGate.ts` and the Go `scanGate` in `simulation_test.go` in step.

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
| 00:12 | D: scan `1008` | set 195 = square, shape 2 = selection | **purged**; selection cleared | `team_scan` +1, `team_pattern` +1, `team_selection` deleted | `pattern_completed`, `pattern ["1001","1002","1007","1008"]` |
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

## The write-through
Every function in `write.go` runs these steps, marked in the code:
1. **lock** the team's entry (`lockTeam`)
2. **validate**:
   - first content and team, **before** the lock (an `IN_GAME` team's status and roster never change). This is AGENTS.md §6 variation (a); the code labels it `2. validate, before 1. lock`
   - then the `State`, under the lock (`2. validate (state)`)
3. **build** the next `State` (a copy)
4. **persist**: one `db.go` call (`insertScan`, `insertCompletingScan`, `upsertSelection` or `deleteSelection`), detached from the request and capped at **5 s** (`storage.write_timeout`)
5. **apply**: `entry.cur.Store(&next)`
6. **publish** `team_state` to every member, still holding the lock so a team's events are in order

**On a failed persist:** `reconcile` reloads the team from Postgres (Postgres wins), publishes the result, and returns the error, which becomes a `500`. Nothing from the failed write is applied.

**Rejections and no-ops return before step 4:** every `409`, and clearing when there's nothing to clear. Nothing is written and nothing is published. Reselecting is **not** a no-op: it always purges, writes and publishes.

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
  "stack": [
    {"waypoint_id": "1001", "user_id": "9a2b…", "scanned_at": "2026-10-17T12:00:01Z", "expires_at": "2026-10-17T12:01:06Z"},
    {"waypoint_id": "1002", "user_id": "c3d4…", "scanned_at": "2026-10-17T12:00:05Z", "expires_at": "2026-10-17T12:01:10Z"}
  ]
}
```
- `selection` is `null` when nothing is selected.
- `stack` is the live scans, bottom (oldest) first; `[]` when empty. Stale correct scans are left out; wrong scans are included (with no flag telling them apart).
- `expires_at` is `scanned_at` + 65 s (window + grace): when a correct scan goes stale on the server. The app's countdown doesn't use it (see [The client countdown and latency](#the-client-countdown-and-latency)).

**`Game`** (`GET /api/game/shapes`), with the 2025 seed and config.toml as shipped:
```json
{
  "shapes": [
    {"id": "collinear", "name": "Collinear", "patterns": 9,   "points": [3]},
    {"id": "lightning", "name": "Lightning", "patterns": 77,  "points": [4]},
    {"id": "square",    "name": "Square",    "patterns": 18,  "points": [4]},
    {"id": "tree",      "name": "Tree",      "patterns": 99,  "points": [4]},
    {"id": "triangle",  "name": "Triangle",  "patterns": 156, "points": [3]}
  ],
  "scan_window_ms": 60000,
  "scan_grace_ms": 5000,
  "max_pattern_points": 6
}
```
- `shapes`: every selectable shape (the client's "types"), in index order (by ID). `id` is what `PUT …/selection` takes; `name` is for display; `patterns` is how many valid patterns the shape has; `points` is the distinct sizes of those patterns, ascending (a shape mixing 3- and 4-point patterns shows `[3, 4]`).
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
- to every member after every change (never after a rejection or no-op)
- right after `team` when a member connects to `GET /api/teams/me/events`

## Rules and cases

### Loading at startup
The backend refuses to start if any of these hold:

| Case |
|---|
| `config.toml` is invalid (see `internal/platform/config`), e.g. a missing key, or `max_pattern_points` > `teams.size` |
| A waypoint ID isn't exactly 4 digits |
| More than 64 waypoints, 64 shapes or 2048 patterns |
| A pattern names an unknown waypoint or shape, has no waypoints, or has more than 6 (`game.max_pattern_points`) |
| Two patterns have the same set of waypoints |
| A pattern is contained in a larger pattern of the same shape |
| Stored progress or a selection refers to a waypoint, pattern or shape that no longer exists |

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
| Stack plus this scan is exactly a pattern of the **selected** shape | `pattern_completed`: pattern and shape marked; **stack and selection purged** |
| ... that pattern was already completed | `pattern_completed` with `already_completed: true`; progress unchanged |
| Stack plus this scan is a pattern of **another** shape | Not completed → `scan_added` |
| Otherwise | `scan_added` |
| 1-point pattern | Completes on its only scan |
| 6-point pattern | Completes on the sixth scan |
| Span exactly 65 s | Completes. 65 s + 1 µs: the bottom is stale and dropped |
| Any accepted scan (wrong included) | Logged in `team_scan` (with `wrong`); waypoint added to visited `waypoints` |
| Postgres write fails | `500`; state reloaded from Postgres; nothing applied |

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
| All stack scans stale | `stack: []` (wrong scans never go stale, so a wrong scan keeps the stack non-empty) |

## Concurrency and locking
- **Writes to one team are serialised** on its `entry.mu`. Different teams run in parallel.
- **Two members scan the same waypoint at once:** exactly one is pushed; the other gets `waypoint_already_scanned`.
- **One member scans two waypoints at once:** exactly one is pushed; the other gets `member_already_scanned`.
- **A phone resends one scan many times at once:** exactly one is pushed and logged; the rest get `waypoint_already_scanned`.
- **A scan and a selection race:** whichever takes the lock first wins. If the selection comes first, the stack is purged and the scan is the first of the new stack.
- **Lock order:** `entry.mu`, then `Store.mu` (only to create an entry). `Store.mu` is never held during I/O.
- **The team itself is read** through `Teams` without the team's lock. That's safe because writes need `IN_GAME`, after which a team's status and roster are frozen.
- **The memory checks are the only enforcement** of the stack rules: they depend on time, which a unique constraint can't express. They all run under `entry.mu`, and Postgres has one writer (the instance lock in `platform/postgres`).

## Failure and recovery
| Case | Result |
|---|---|
| Postgres times out or drops during a write | `500`. `reconcile` re-reads the team (the write may or may not have committed) and memory matches Postgres |
| Memory and Postgres disagree (rows edited by hand) | The next write that notices (e.g. `deleteSelection` finding no row) fails, reconciles, and members get the corrected state |
| Postgres unreachable | Reads keep working from memory; writes fail |
| Backend restarts | Progress, selections and live stacks reload exactly; scans keep their original times, so they go stale when they would have |
| `config.toml` changes between restarts | A longer window can revive scans that were stale; a smaller capacity loads fewer scans. Both apply consistently from then on |
| Content edited while running | Not seen until restart. Don't. |

## Limits and how to change them
| Limit | Where | To change |
|---|---|---|
| Scan window 1 m | `config.toml` `game.scan_window` | Edit and restart |
| Grace 5 s | `config.toml` `game.scan_grace` (0 allowed, but the key must be present) | Edit and restart |
| 6 points per pattern / stack capacity | `config.toml` `game.max_pattern_points` (≤ `teams.size`) | Edit and restart. Content with bigger patterns is refused at startup |
| 16 slots in a `Stack` | `config.MaxTeamSize` | Fixed array size; raise with the team-size cap |
| 64 waypoints | `WaypointSet uint64`, `maxWaypoints` | Make it `[2]uint64` (arrays are still valid map keys, so `patternBySet` keeps working), update the bit methods, test across the word boundary |
| 64 shapes | `ShapeSet uint64`, `maxShapes` | Same approach |
| 2048 patterns | `PatternSet [32]uint64`, `maxPatterns` | Make the array longer |

## Not decided yet
- **⚠ QR verification.** `POST …/scans` accepts the raw 4-digit waypoint ID, so a player can claim a scan without being there. This must be replaced before the event (rotating or signed QR codes, verified server-side).
- **Whether the leader alone should select.** Currently any member can.

## Tests
- `content_test.go` (no database). A four-waypoint fixture: triangles `1001 1002 1003` and `1002 1003 1004`, line `1001 1004`. `game.max_pattern_points` comes from `config.toml`; below the fixture's 3 points (`fixtureMaxPoints`) these tests **skip**:

  | Test | Covers |
  |---|---|
  | `TestPatternLookupIgnoresScanOrder` | a pattern is found from its waypoints in any order; partial, extra and empty sets match nothing; an unknown waypoint is an error |
  | `TestPatternIndexes` | waypoint and shape indexes in ID order; each shape's patterns; patterns by Postgres key; `WaypointIDs` sorts |
  | `TestContentValidation` | each startup refusal, by its error: ID not 4 digits (`A1`, `10001`), unknown waypoint, unknown shape, empty pattern, two patterns with the same waypoints, a pattern over `game.max_pattern_points`, a pattern nested in a larger one of its shape, more than 64 waypoints, 64 shapes, 2048 patterns |
  | `TestBitsets` | `PatternSet` across 64-bit words (bits 0, 63, 64, 70, 2047); `WaypointSet` `With`, `Has`, `Len`, `Contains` |
  | `TestMixedSizesAllowedWhenNotNested` | a shape may mix 2- and 3-point patterns when none is inside another |
  | `TestStack` | bottom-to-top order, overflow dropping the bottom, `Push` leaving the original alone, `Live` at exactly the limit, and a wrong scan staying live however old it is |
  | `TestFitsShape` | one waypoint in either triangle, a pair in both, a pair in none, a whole pattern, a waypoint on no line, a pair on a line, and the empty set → `false` |

- `store_test.go` (real Postgres, config.toml's rules, fake clock). Times are written relative to the window (`tick`, a 60th of `game.scan_window`) or to the limit (`rules.limit()`), so each scenario means the same under any config. The tests use up to 6 distinct members (the 6-point pattern needs all of them), so they need `teams.size` ≥ 6 (`minTestTeam`); with a smaller size in `config.toml` they **skip** rather than fail:

  | Test | Covers |
  |---|---|
  | `TestDevSeedLoads` | seed loads under config.toml's rules and re-seeds |
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
  | `BenchmarkScanLookups` | `FitsShape`, `PatternFor` and `WaypointIndex`+`Has` on the `content_test.go` fixture (not the seed), results kept in package-level sinks. On an i7-12650H: about 1 ns, 2 ns and 6 ns |
- `internal/server/teamstate_test.go`: `TestSelectAndScanRoutes`, every route and error code over HTTP, including `waypoint_already_scanned`, `member_already_scanned`, a wrong scan being accepted and locking the member, and the body size limit; `TestGameShapes`, `GET /api/game/shapes` without a token (`401 missing_token`) and with one: the seed's 5 shapes, `square` with 18 patterns of 4 points, and the window, grace and capacity from `config.toml`.
- `internal/server/simulation_test.go`: the countdown under latency (see [The client countdown and latency](#the-client-countdown-and-latency)).
- `internal/server/events_test.go`: `team_state` on connect.
