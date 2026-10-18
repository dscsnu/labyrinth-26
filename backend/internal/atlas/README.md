# atlas

The game's static content: the waypoints (QR locations), the shapes a team can select, the patterns (sets of waypoints) that form each shape, and the spell each pattern casts. Loaded from Postgres once at startup, validated, held in memory, and never changed while the backend runs. A **read-only use case**: no writes at runtime and no routes of its own.

Every number below that comes from `config.toml` is shown with its current value and its key, e.g. **6** (`game.max_pattern_points`). Change the file, not the code.

## Contents
1. [Concepts](#concepts)
2. [The flow](#the-flow)
3. [How each file works](#how-each-file-works)
4. [Data model: memory](#data-model-memory)
5. [Data model: Postgres](#data-model-postgres)
6. [Memory ↔ Postgres](#memory--postgres)
7. [How waypoints and patterns are stored](#how-waypoints-and-patterns-are-stored)
8. [Spells](#spells)
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
The client, the code and the spell PR use different words. The code's are used here:

| Client says | PR (`feat/damage-lookup`) says | Code says | Meaning |
|---|---|---|---|
| type | | `Shape` | square, triangle, …; what a team selects |
| shape | | `Pattern` | one valid set of 4-digit waypoint IDs of a shape |
| point | | `Waypoint` | a QR location, 4-digit ID |
| | spell, powerup, damage | `Spell` | what completing a pattern casts: the team's reward |

| Term | Meaning |
|---|---|
| **Waypoint** | A physical location with a QR code. Identified by a **4-digit ID** (`"1001"`; leading zeros allowed). Has a display name (`"Tower 6 FruitShop (T6)"`) and WGS 84 latitude and longitude. |
| **Shape** | What a pattern looks like: `collinear`, `lightning`, `square`, `tree`, `triangle` in the 2025 seed. Identified by a lowercase slug. A shape has many patterns. |
| **Pattern** | A set of **1 to 6** (`game.max_pattern_points`) waypoints that forms a shape on the map. Identified **by that set**, in any order. Its Postgres `id` is only a key (what `team_pattern` stores). |
| **Index** | A waypoint's, shape's or pattern's position in the atlas's slices. Exists only in memory and may differ between restarts. Postgres, the API and live events always use IDs. |
| **Set** | A bitmask over indexes: `WaypointSet`, `ShapeSet`, `PatternSet`. Bit *i* means item *i*. |
| **Nested pattern** | A pattern contained in a larger pattern of the same shape. Refused at startup: scanning the larger one would pass through the smaller one first and complete it early. |
| **Spell** | What completing a pattern casts: `Spell{Damage, Geometry}`, one per pattern, computed once at startup. **Not computed yet**: zero on main (see [Spells](#spells)). |
| **Position** | A waypoint on a flat local plane, in metres east (X) and north (Y) of an origin. **Not computed yet**: zero on main. |
| **Read-only use case** | Static content, no runtime writes, no routes. It has `doc.go`, `read.go`, `db.go`, `store.go` and this README, and no `api.go` or `write.go` (AGENTS.md §3). |

## The flow
1. **Content is written** by migrations and seeds (`go run . seed` loads the 2025 placeholder), with the backend stopped. The atlas never writes.
2. **`server.Build` calls `atlas.Load` first**, before any use case that reads it, with `atlas.Rules{MaxPatternPoints: game.max_pattern_points}`.
3. **`Load` reads** every waypoint, shape and pattern (`db.selectAtlas`), **validates** them (`New`), and builds the lookup tables. Bad content refuses startup.
4. **(Not yet) `Load` computes spells**: each `Waypoint.Position`, then each `Pattern.Spell` (see [Spells](#spells)).
5. **`Load` logs** `msg="loaded atlas" waypoints=11 shapes=5 patterns=359` (2025 seed) and returns the `*Atlas`.
6. **Other use cases read it** through small interfaces of their own. teamstate declares `teamstate.Atlas` and uses it to validate selections and scans, label wrong scans (`FitsShape`), detect completions (`PatternFor`), rebuild stored state, and build `GET /api/game/shapes`.

**Why a use case of its own.** Content is the same for every team and never changes while running; team state changes on every scan. Keeping them apart means the static part has no locks, no write-through and no reconcile, and the spell model (pure geometry over content) has a home that isn't teamstate's write path.

**Why no locks.** Everything is built before `Load` returns and nothing modifies it after, so any goroutine may read it concurrently (Go's memory model: the `*Atlas` is published to readers by `server.Build` before the server starts).

**Why sets, not lists.** A scan asks "is this waypoint already in?" and "do these waypoints form a pattern?". As bitmasks those are one AND and one map lookup keyed by the set itself, and scan order doesn't matter (see [How waypoints and patterns are stored](#how-waypoints-and-patterns-are-stored)).

## How each file works

### `doc.go`
The package comment: what the atlas owns, that it's static and loaded once, that other use cases read it through their own interfaces. Points here.

### `sets.go`: set types and capacities
- **Constants** (Go type sizes, not tunables, AGENTS.md §10): `MaxWaypoints = 64`, `MaxShapes = 64`, `MaxPatterns = 2048`.
- **`WaypointSet`** (`uint64`): `Has(i)`, `With(i)`, `Contains(o)` (`s&o == o`), `Len()` (`bits.OnesCount64`), `Indexes()` (ascending). Comparable, so it keys `patternBySet`.
- **`ShapeSet`** (`uint64`): `Has`, `With`, `Indexes`.
- **`PatternSet`** (`[32]uint64`): `Has`, `With`, `Indexes`. An array, so a value copy (a teamstate snapshot) copies it without allocating; `With` works on the receiver copy and leaves the original unchanged.
- **`setBits(word, out, offset)`** appends `offset` + each set bit's position, lowest first (`bits.TrailingZeros64`). Used by every `Indexes`.
- All methods are on values; none modifies its receiver. Callers: teamstate (`State` holds one of each) and `New`.

### `read.go`: types and lookups
- **Types:** `Waypoint`, `Point`, `Shape`, `Pattern`, `Spell`, `Geometry`, `Atlas` (see [Data model: memory](#data-model-memory)).
- **`Geometry.Convex(set)`**: `Hull == set`, every waypoint of the set on its hull. Unused until spells are computed.
- **Counts:** `WaypointCount()`, `ShapeCount()`, `PatternCount()`. Indexes run from 0 to count − 1.
- **By index:** `Waypoint(i)`, `Shape(i)`, `Pattern(i)` return copies. An out-of-range index panics (slice indexing); callers only pass indexes the atlas gave them.
- **By ID:** `WaypointIndex(id)`, `ShapeIndex(id)`, `PatternByID(id)` (Postgres key) return `(index, ok)`.
- **`SetOf(ids)`**: IDs to a `WaypointSet`; `unknown waypoint "9999"` on the first unknown ID. Duplicates collapse (it's a set). Used by `New` and teamstate's `stateOf`.
- **`PatternFor(set)`**: the pattern made of **exactly** these waypoints, one map lookup. Teamstate's completion check.
- **`ShapePatterns(shape)`**: the shape's pattern indexes, in key order. The slice is shared: callers must not modify it. Used by `FitsShape`, `checkNesting` and teamstate's `newGame`.
- **`FitsShape(shape, set)`**: whether some pattern of the shape contains every waypoint in `set` (see [Does a set still fit the shape?](#does-a-set-still-fit-the-shape)). The empty set fits nothing. Teamstate's wrong-scan check.
- **`WaypointIDs(set)`**: a set back to IDs, sorted. Used for every view and `ScanResult.Pattern`, and the string key for a pattern ([Spells](#spells), identity).
- No lookup locks or touches Postgres. Only `WaypointIDs` and the `Indexes` methods allocate (their result slices).

### `store.go`: layout, `Rules`, `Load`, validation
- **Layout comment** drawing the `Atlas` (below).
- **`waypointIDPattern`** `^[0-9]{4}$`: a format, not a tunable; matches the `waypoint.id` CHECK in migration 0003.
- **`Rules`**: `MaxPatternPoints` (`game.max_pattern_points`) and `Spell` (`SpellRules`, not in config.toml yet; zero).
- **`SpellRules`**: `PerPoint`, `Per100m`, `Walkability` (see [Spells](#spells)). Unread.
- **`Load(ctx, pg, log, rules)`**: `selectAtlas` (error → `load atlas: …`), then `New(src, rules)` (error → `invalid atlas: …`), then the marked comment where spells will be computed, then the `loaded atlas` log line. Called once, by `server.Build`, and by the tests.
- **`New(src, rules)`**: builds an Atlas from a `Source`, validating in the order of [Loading at startup](#loading-at-startup) and builds every slice and map. Waypoints and shapes take their index from their row order (ID order, from the query); patterns from theirs (key order).
- **`checkNesting(shape, patterns)`**: for every pair of the shape's patterns, refuses one that's smaller than and contained in another. O(n²) per shape: 156² ≈ 24,000 AND-compares for the seed's triangles, once at startup.

### `db.go`: SQL (reads only)
- **`db{pool}`**: the pool. The atlas never writes; content changes only through migrations and seeds, with the backend stopped.
- **`Source`** (`Waypoints []Waypoint`, `Shapes []Shape`, `Patterns []PatternSource`) and **`PatternSource`** (`ID`, `Shape` (the shape's ID), `Waypoints []string`): the content tables as stored, by IDs. `Load` reads one from Postgres; tests build one by hand and pass it to `New`. Exported for that reason (AGENTS.md §13).
- **`selectAtlas(ctx)`** runs three queries on the pool, not in a transaction (nothing writes content while the backend runs, and the instance lock keeps `seed` out):
  1. `SELECT id, name, latitude, longitude FROM waypoint ORDER BY id`
  2. `SELECT id, name FROM shape ORDER BY id`
  3. `SELECT p.id, p.shape_id, COALESCE(array_agg(pw.waypoint_id ORDER BY pw.waypoint_id) FILTER (WHERE pw.waypoint_id IS NOT NULL), '{}') FROM pattern p LEFT JOIN pattern_waypoint pw ON pw.pattern_id = p.id GROUP BY p.id ORDER BY p.id`. The `LEFT JOIN` and `COALESCE` return a pattern with no waypoints as `{}`, so `New` can refuse it instead of it vanishing.

### `tests/store_test.go`
See [Tests](#tests).

### `geometry.go` (to add)
Pure maths for spells, no I/O and no `Atlas` state: the projection, the shortest closed tour, the convex hull. Listed here per AGENTS.md §3 (supporting files must be in the README). See [Spells](#spells).

## Data model: memory
```
Atlas                                built once by Load; never modified after
  waypoints      []Waypoint          index → waypoint, ID order
  shapes         []Shape             index → shape, ID order
  patterns       []Pattern           index → pattern, Postgres key order
  waypointIndex  map[string]int      "1007" → 6
  shapeIndex     map[string]int      "square" → 2
  patternByID    map[int]int         Postgres key → index (87 → 86)
  patternBySet   map[WaypointSet]int exact set → index (195 → 86)
  shapePatterns  [][]int             shape index → its pattern indexes, key order
```

| Struct | Field | Go type | Meaning |
|---|---|---|---|
| `Waypoint` | `ID` | `string` | 4 digits, e.g. `"1001"`; identity everywhere (JSON `id`) |
| | `Name` | `string` | display, e.g. `"Tower 6 FruitShop (T6)"` (JSON `name`) |
| | `Latitude` | `float64` | degrees, WGS 84, from Postgres (JSON `latitude`) |
| | `Longitude` | `float64` | degrees, WGS 84, from Postgres (JSON `longitude`) |
| | `Position` | `Point` | metres on the campus plane; **zero until spells are computed**; never in JSON (`json:"-"`) |
| `Point` | `X`, `Y` | `float64` | metres east, north of a fixed origin |
| `Shape` | `ID` | `string` | what a selection names, e.g. `"square"` (JSON `id`) |
| | `Name` | `string` | display, e.g. `"Square"` (JSON `name`) |
| `Pattern` | `ID` | `int` | Postgres `pattern.id`; what `team_pattern` stores |
| | `Shape` | `int` | its shape's index |
| | `Waypoints` | `WaypointSet` | its waypoints |
| | `Spell` | `Spell` | what completing it casts; **zero until computed** |
| `Spell` | `Damage` | `int` | what the spell deals |
| | `Geometry` | `Geometry` | what `Damage` is computed from, kept for display and tests |
| `Geometry` | `Hull` | `WaypointSet` | waypoints on the set's convex hull; interior points are `pattern.Waypoints &^ Hull` |
| | `TourM` | `float64` | shortest closed loop through every waypoint, straight-line metres (out and back for 2, 0 for 1) |
| | `WalkM` | `float64` | `TourM × Walkability`: estimated walking metres |

- **Size**, from `unsafe.Sizeof` on amd64: `Waypoint` 64 bytes, `Shape` 32, `Pattern` 56 (of which `Spell` 32). With the 2025 seed the three slices are 11 × 64 + 5 × 32 + 359 × 56 = **20,968 bytes**, plus the strings and the five maps: tens of KB. At the limits (64 waypoints, 64 shapes, 2048 patterns) the slices are about 121 KB.
- **Why slices plus maps.** Teamstate stores indexes in fixed-size bitsets, so it needs index → item (slices) and ID → index (maps). Patterns are found by set, not ID, during play, hence `patternBySet`; `patternByID` is only for loading `team_pattern`.
- **Why `Pattern` holds `Spell` by value.** A scan that completes a pattern reads `Pattern(i).Spell` as a field: no map, no computation.
- **No `json` tags on `Pattern`, `Spell`, `Geometry`, `Point`.** No route serialises them today.

## Data model: Postgres
Migration `0003_content_progress.sql`. The atlas reads these tables; nothing at runtime writes them.

| Table | Column | Type | Constraint | Rule it backs |
|---|---|---|---|---|
| `waypoint` | `id` | `TEXT` | PRIMARY KEY, `CHECK (id ~ '^[0-9]{4}$')` | 4-digit IDs, unique; printed QR codes and stored progress never change meaning. `New` checks the same pattern |
| | `name` | `TEXT` | NOT NULL | |
| | `latitude`, `longitude` | `DOUBLE PRECISION` | NOT NULL | the only source of coordinates ([Spells](#spells)); no range check |
| `shape` | `id` | `TEXT` | PRIMARY KEY, `CHECK (id ~ '^[a-z0-9_-]{1,32}$')` | lowercase slug, unique |
| | `name` | `TEXT` | NOT NULL | |
| `pattern` | `id` | `INTEGER` | PRIMARY KEY | key `team_pattern` refers to |
| | `shape_id` | `TEXT` | NOT NULL, REFERENCES `shape (id)` | a pattern's shape exists (`New` checks it too) |
| `pattern_waypoint` | `pattern_id` | `INTEGER` | NOT NULL, REFERENCES `pattern (id)` ON DELETE CASCADE | deleting a pattern removes its waypoint list |
| | `waypoint_id` | `TEXT` | NOT NULL, REFERENCES `waypoint (id)` | a pattern's waypoints exist (`New` checks it too) |
| | | | PRIMARY KEY `(pattern_id, waypoint_id)` | a waypoint is in a pattern at most once |

**Not constraints, checked only by `New`:** a pattern has at least one waypoint, at most `game.max_pattern_points`; no two patterns share a set; no pattern is nested in a larger one of its shape; the counts fit the set types. They span rows or depend on config, which a constraint can't express.

Teamstate's tables refer to these (`team_scan.waypoint_id`, `team_pattern.pattern_id`, `team_selection.shape_id`); see `internal/teamstate/README.md`.

## Memory ↔ Postgres
| Postgres | Rebuilt in memory by `Load` |
|---|---|
| `waypoint` (ID order) | `waypoints`, `waypointIndex` |
| `shape` (ID order) | `shapes`, `shapeIndex` |
| `pattern` + `pattern_waypoint` (key order) | `patterns` (`ID`, `Shape`, `Waypoints`), `patternByID`, `patternBySet`, `shapePatterns` |
| (none: computed from `latitude`/`longitude`) | `Waypoint.Position`, once spells are added |
| (none: computed from positions and `Rules.Spell`) | `Pattern.Spell`, once spells are added |

- **Exact on every start.** Every field is either read from Postgres or a pure function of what was read and of `config.toml`. The same rows and config give the same atlas, indexes included (the queries are ordered).
- **Not persisted, by design:** indexes, the maps, positions and spells. They're derived; storing them would be a second copy that can drift (see [Spells](#spells), single source of truth).
- **Changes need a restart.** Content edited while the backend runs isn't seen until the next start.

## How waypoints and patterns are stored

### Indexes
At startup every waypoint, shape and pattern gets an **index**: its position in an `Atlas` slice.
- Waypoints and shapes by ID; patterns by Postgres key.
- 2025 seed: waypoints `1001`→0 … `1011`→10; shapes `collinear`→0, `lightning`→1, `square`→2, `tree`→3, `triangle`→4; patterns `1`→0 … `359`→358 (keys are 1–359 with no gaps).
- Indexes exist **only in memory**. Postgres, the API and live events always use IDs.

### Sets are bitmasks
Bit *i* means item *i*:

| Type | Go type | Holds |
|---|---|---|
| `WaypointSet` | `uint64` | up to 64 waypoints |
| `ShapeSet` | `uint64` | up to 64 shapes |
| `PatternSet` | `[32]uint64` | up to 2048 patterns |

Example: the square `1001, 1002, 1007, 1008` (pattern `87`) is indexes 0, 1, 6, 7, so its `WaypointSet` is `0b11000011` = **195**. Any scan order sets the same bits.

### Lookup tables
| Field | Maps | Used for |
|---|---|---|
| `waypoints`, `shapes`, `patterns` | index → item | turning indexes back into IDs |
| `waypointIndex` | `"1007"` → 6 | validating a scan, `SetOf` |
| `shapeIndex` | `"square"` → 2 | validating a selection |
| `patternBySet` | `195` → 86 (pattern `87`, the square) | the completion check, one map lookup (`PatternFor`; about 2 ns on the test fixture, `BenchmarkLookups`) |
| `patternByID` | `87` → 86 | loading `team_pattern` (teamstate's `stateOf`) |
| `shapePatterns` | `2` → the square's 18 pattern indexes | `FitsShape`, `checkNesting`, teamstate's `newGame` (each shape's count and sizes) |

### Bit operations
| Question | Operation |
|---|---|
| Visited 1007? | `set & (1<<6) != 0` (`Has`) |
| Stack plus this scan | `set \| 1<<w` (`With`) |
| Still inside a pattern of the shape? | any `p` of the shape with `p & set == set` (`Contains`, in `FitsShape`) |
| Stack *is* a pattern? | `patternBySet[set]` (exact, `PatternFor`) |
| How many points? | `bits.OnesCount64(set)` (`Len`) |
| Interior points of a pattern | `p.Waypoints &^ p.Spell.Geometry.Hull` |

### Does a set still fit the shape?
`FitsShape(shape, set)`: is there a pattern of the shape containing every waypoint in `set`? Teamstate calls it with the stack plus the new scan; `false` marks the scan **wrong**, it doesn't reject it.

1. The empty set fits nothing (`false` at once).
2. For each index `p` in `shapePatterns[shape]`: if `patterns[p].Waypoints & set == set`, return `true`.
3. None matched: `false`.

One AND-compare per pattern of the shape, with an early exit. The 2025 seed:

| Shape | Patterns | Points |
|---|---|---|
| `collinear` | 9 | 3 |
| `lightning` | 77 | 4 |
| `square` | 18 | 4 |
| `tree` | 99 | 4 |
| `triangle` | 156 | 3 |

Measured once on the 2025 seed, by hand (the committed `BenchmarkLookups` runs on the small test fixture instead): about **1 ns** on a hit (early exit), about **9 ns** on a miss (all 156 triangles checked).

**Why not a lookup table.** The alternative is a precomputed map from every `(shape, subset of one of its patterns)` to `true`. On the seed that's **983** entries, and, measured once on the seed, a flat **15–19 ns**: hashing a key costs more than scanning at most 156 `uint64`s. So the loop stays. Revisit only if a shape grows to thousands of patterns.

## Spells
The contract for the spell model, an open PR (branch `feat/damage-lookup`) to be redone against this package. Nothing below is computed on main yet; the types are in place and zero.

### What a spell is
What completing a pattern casts: the team's reward for it (the PR calls it damage or powerup). **One per pattern**, held in memory on `Pattern.Spell`, computed once at startup, so a scan that completes a pattern reads it as a field.

### The structs already in place, unfilled
| Where | What | Today |
|---|---|---|
| `Waypoint.Position` | `Point{X, Y}`: metres east (X) and north (Y) of an origin | zero |
| `Pattern.Spell` | `Spell{Damage int, Geometry Geometry}` | zero |
| `Spell.Geometry` | `Geometry{Hull WaypointSet, TourM float64, WalkM float64}` | zero |
| `Geometry.Convex(set)` | `Hull == set`; interior points are `pattern.Waypoints &^ Hull` | unused |
| `Rules.Spell` | `SpellRules{PerPoint, Per100m, Walkability float64}` | zero; `server.Build` doesn't set it |

Nothing reads any of them yet.

### Where to compute them
In `Load` in `store.go`, **at the marked comment, after `New` validates**:
1. **Every `Waypoint.Position`** from latitude and longitude, with the PR's projection: equirectangular around the waypoints' mean. With the origin (lat₀, lon₀) = the mean of all waypoints' coordinates and *R* the Earth's radius (a named constant whose comment gives its value and why it isn't in config.toml, AGENTS.md §10):
   - x (east) = *R* · (lon − lon₀) · cos(lat₀), in radians
   - y (north) = *R* · (lat − lat₀)
2. **Every `Pattern.Spell`** from its waypoints' positions and `rules.Spell`:
   - `Hull`: the waypoints on the convex hull of their positions (collinear points on an edge: as the PR's hull tests decide)
   - `TourM`: the shortest closed loop through every waypoint (out and back for 2, 0 for 1). With at most 6 points, every order can be tried: at most 5!/2 = 60 distinct loops
   - `WalkM` = `TourM × Walkability`
   - `Damage` = `PerPoint × points + Per100m × WalkM / 100` (the field comments on `SpellRules`); its rounding to `int` is stated here when the PR lands

- **Pure maths** (projection, tour, hull) goes in a new supporting file **`geometry.go`**, listed in [How each file works](#how-each-file-works) per AGENTS.md §3. `Load` only loops and assigns.
- **Immutable after `Load` returns.** Positions and spells are written into the slices before `Load` returns the `*Atlas`, never after; no lock is added.
- Positions and spells live only in memory and are rebuilt from Postgres on every start ([Memory ↔ Postgres](#memory--postgres)).

### Identity
- Waypoints by **4-digit ID**; patterns by **`WaypointSet`** (in memory) or Postgres **`ID`**.
- **Never short names** like `"T6"`: they aren't stored. They only appear inside display names (`"Tower 6 FruitShop (T6)"`).
- If a string key is needed (logs, test tables, JSON), it's the pattern's sorted waypoint IDs joined by `-`: `strings.Join(a.WaypointIDs(p.Waypoints), "-")` → `"1001-1002-1007-1008"`.

### Single source of truth
- Coordinates come **only from Postgres** (`waypoint.latitude`, `waypoint.longitude`), never from a JSON file in the repo.
- **No precomputed tables** in the repo: spells are computed at startup from what Postgres holds.
- Why: the PR's copy of T6's longitude is `77.5777379378554`; the seed's is `77.57777379378554` (a dropped digit). That's about **3.5 m** east–west at campus latitude: exactly the drift a second copy causes.

### Config
The coefficients go in `config.toml`, following AGENTS.md §10's steps:
1. A **`[spell]`** table, each key commented with what it does and its range:
   - `per_point`: damage per waypoint in the pattern. Proposed range: ≥ 0.
   - `per_100m`: damage per 100 m of `WalkM`. Proposed range: ≥ 0.
   - `walkability`: straight-line to walking distance. Proposed range: ≥ 1 (a walk is never shorter than the straight line).
2. A `Spell` struct in `internal/platform/config/settings.go` with `toml` tags, doc comments and range checks in `validate`.
3. `validTOML` in `internal/platform/config/tests/settings_test.go`, and `TestLoadSettingsRejects` cases for out-of-range values.
4. **Passed through `atlas.Rules.Spell`** in `server.Build`: `atlas.Rules{MaxPatternPoints: …, Spell: atlas.SpellRules{PerPoint: …, Per100m: …, Walkability: …}}`.
5. Tests read the values with `configtest.Settings(t)`; docs write each value with its key. `internal/platform/README.md` and `internal/server/README.md` get the new keys.

### Coverage
- Spells are computed for **the patterns in the atlas**: **359** in the 2025 seed, sizes **3–4** (165 of 3 points, 194 of 4). Not for every subset of waypoints.
- The code must still handle every allowed size, 1 to `game.max_pattern_points`: a **1-point** pattern has tour 0, so damage = `per_point`; a 2-point one goes out and back.

### Tests expected
In `internal/atlas/geometry_test.go` (pure functions) and `tests/store_test.go` (through `Load`):
- **Geometry on the seed's real coordinates through `Load`**: the PR's hull-count and non-collinearity checks, 6-point sets included (built from seed waypoints, since the seed's patterns stop at 4).
- **Every pattern has a non-zero spell** once computed (with config.toml's coefficients, skipping if they're zero).
- Plus AGENTS.md §13's edges: a 1-point pattern (`Damage == per_point`), a 2-point one (tour = twice the distance), a collinear set.

### Mapping from the PR
| PR file | Here |
|---|---|
| `damage_lookup.json` | `Pattern.Spell`, computed in `Load` |
| `labyrinth_point_combinations.json` | deleted: Postgres has the coordinates |
| `geometry_test.go` | `internal/atlas/tests/geometry_test.go`, testing `geometry.go` through the exported API (`atlas.New` / `atlas.Load`) |
| `DAMAGE_MODEL.md` | this section |

Open questions are under [Not decided yet](#not-decided-yet).

## Walkthroughs
2025 seed, `game.max_pattern_points` = **6**.

### 1. Loading the seed
| Step | Input | Result |
|---|---|---|
| `selectAtlas` | 11 waypoint rows `1001`…`1011`, 5 shape rows, 359 pattern rows | `Source` |
| counts | 11 ≤ 64, 5 ≤ 64, 359 ≤ 2048 | ok |
| waypoints | `1001` matches `^[0-9]{4}$` … | `waypointIndex["1001"] = 0` … `["1011"] = 10` |
| shapes | `collinear` … `triangle` | `shapeIndex["square"] = 2` |
| pattern row `{87, "square", ["1001","1002","1007","1008"]}` | shape known; `SetOf` → 195; 4 ≤ 6; 195 not seen | `patterns[86] = {ID 87, Shape 2, Waypoints 195}`; `patternByID[87] = 86`; `patternBySet[195] = 86`; `shapePatterns[2]` += 86 |
| pattern row `{208, "triangle", ["1001","1002","1007"]}` | `SetOf` → 67 | `patternBySet[67] = 207`; `shapePatterns[4]` += 207 |
| `checkNesting` | 67 ⊂ 195, but triangle ≠ square | ok: nesting is only refused within one shape |
| log | | `msg="loaded atlas" waypoints=11 shapes=5 patterns=359` |

### 2. Lookups during a square (teamstate's calls)
Team selected `square` (shape 2):

| Scan | Set | `FitsShape(2, set)` | `PatternFor(set)` | Teamstate does |
|---|---|---|---|---|
| `1001` | 1 | true | none | `scan_added` |
| `1002` | 3 | true | none | `scan_added` |
| `1007` | 67 | true (inside 195) | 207, a **triangle** (shape 4 ≠ 2) | `scan_added`: wrong shape, no completion |
| `1008` | 195 | true | 86, shape 2 | `pattern_completed`, `WaypointIDs(195)` = `["1001","1002","1007","1008"]`; `team_pattern` stores `Pattern(86).ID` = 87 |

Had the third scan been `1003` instead: set `0b111` = 7, and no square contains 1001, 1002 and 1003, so `FitsShape` is `false` and teamstate marks it wrong.

### 3. A refused start
A migration adds pattern `400`, shape `square`, waypoints `1001 1002 1007` (set 67, a subset of square 87's 195):

```
level=ERROR msg=fatal error="invalid atlas: shape \"square\": pattern 400 is inside pattern 87, so it would complete first"
```
(`main` logs it with the `slog` text handler, after the `time=` field, and exits `1`.)
A team scanning square 87 would hit 67 on its third scan and complete 400 instead. Fix the content, restart.

## The write-through
Not applicable: the atlas has no writes at runtime. Content changes only through migrations and seeds with the backend stopped.

## API
Not applicable: the atlas has no routes of its own. `GET /api/game/shapes` is served by teamstate, from the atlas (shapes, pattern counts and sizes) plus its timing rules; see `internal/teamstate/README.md`.

## Rules and cases

### Loading at startup
`Load` returns an error, and the backend refuses to start, in these cases, **checked in this order** (the first one found is reported):

| # | Case | Error |
|---|---|---|
| 1 | A query fails (database unreachable, table missing) | `load atlas: …` |
| 2 | More than 64 waypoints | `invalid atlas: 65 waypoints; at most 64 fit in a WaypointSet` |
| 3 | More than 64 shapes | `invalid atlas: 65 shapes; at most 64 fit in a ShapeSet` |
| 4 | More than 2048 patterns | `invalid atlas: 2049 patterns; at most 2048 fit in a PatternSet` |
| 5 | A waypoint ID isn't exactly 4 digits (first in ID order) | `invalid atlas: waypoint id "A1" is not 4 digits` |
| 6 | A pattern names an unknown shape (patterns in key order; 6–10 are per pattern) | `invalid atlas: pattern 10: unknown shape "hexagon"` |
| 7 | A pattern names an unknown waypoint | `invalid atlas: pattern 10: unknown waypoint "9999"` |
| 8 | A pattern has no waypoints | `invalid atlas: pattern 10 has no waypoints` |
| 9 | A pattern has more than 6 (`game.max_pattern_points`) waypoints | `invalid atlas: pattern 98 has 7 waypoints; game.max_pattern_points is 6` |
| 10 | Two patterns have the same set of waypoints (in any order, any shapes) | `invalid atlas: patterns 10 and 99 use the same waypoints` |
| 11 | A pattern is inside a larger pattern of the same shape (shapes in index order) | `invalid atlas: shape "triangle": pattern 97 is inside pattern 10, so it would complete first` |

**Allowed, at the boundaries:**

| Case | Result |
|---|---|
| Exactly 64 waypoints, 64 shapes, 2048 patterns | Loads |
| A pattern of exactly `game.max_pattern_points` waypoints | Loads |
| A 1-point pattern | Loads (unless it's inside a larger pattern of its shape) |
| One shape mixing sizes (2- and 3-point patterns), none inside another | Loads |
| A pattern inside a larger pattern of **another** shape (the seed's triangle 67 inside square 195) | Loads: only the selected shape can complete |
| A shape with no patterns | Loads; `GET /api/game/shapes` shows it with `patterns: 0` |
| A waypoint in no pattern | Loads; scanning it is always a wrong scan |
| No content at all (empty tables) | Loads, with 0 of each |
| Latitude or longitude out of range (e.g. `0, 0`) | Loads: not checked (see [Not decided yet](#not-decided-yet)) |

Conditions Postgres already prevents (duplicate IDs, a bad shape slug, a pattern listing a waypoint twice, dangling references) aren't re-checked except where `New` needs the answer anyway (unknown shape or waypoint).

Stored team progress that refers to content that no longer exists is refused later, by teamstate's `Load` (`internal/teamstate/README.md`).

### Lookups
| Call | Case | Result |
|---|---|---|
| `WaypointIndex`, `ShapeIndex`, `PatternByID` | Unknown ID | `(0, false)` |
| `SetOf(ids)` | An unknown ID | `unknown waypoint "9999"` (first unknown) |
| | Repeated IDs | Counted once |
| | Empty list | `0`, no error |
| `PatternFor(set)` | Exactly a pattern's set, any scan order | `(index, true)` |
| | A subset, a superset, or empty | `(0, false)` |
| `FitsShape(shape, set)` | Empty set | `false` |
| | Inside at least one of the shape's patterns | `true` |
| | Spans two patterns, or touches none | `false` |
| `Waypoint(i)`, `Shape(i)`, `Pattern(i)`, `ShapePatterns(i)` | `i` out of range | panics (callers only pass indexes from the atlas) |

## Concurrency and locking
No races to resolve: the atlas is built once before `Load` returns and never modified, so any goroutine reads it with no lock. `ShapePatterns` returns a shared slice; callers must not modify it.

## Failure and recovery
| Case | Result |
|---|---|
| Bad content (any case in [Loading at startup](#loading-at-startup)) | `server.Build` returns the error; the backend exits `1` before serving. Fix the content (a new migration, or edit and reseed with the backend stopped), restart |
| Postgres unreachable at startup | `load atlas: …`; exit `1` |
| Postgres down while serving | No effect: the atlas never reads Postgres after `Load` |
| Content edited while running | Not seen until restart. Don't: teamstate may store `team_pattern` keys or `team_scan` waypoints that the new content no longer has, and the next start refuses |
| `game.max_pattern_points` lowered between restarts below the largest pattern | The next start refuses (case 9) |
| Restart | The atlas is rebuilt exactly (same rows, same order, same indexes) |

## Limits and how to change them
| Limit | Where | To change |
|---|---|---|
| 1 to 6 waypoints per pattern | `config.toml` `game.max_pattern_points` (1 to `teams.size`) | Edit and restart. Content with bigger patterns is refused |
| 64 waypoints | `WaypointSet uint64`, `MaxWaypoints` | Make it `[2]uint64` (arrays are still valid map keys, so `patternBySet` keeps working), update the bit methods, test across the word boundary |
| 64 shapes | `ShapeSet uint64`, `MaxShapes` | Same approach |
| 2048 patterns | `PatternSet [32]uint64`, `MaxPatterns` | Raise `MaxPatterns` (a multiple of 64); the array grows with it, and so does every teamstate `State` copy (256 bytes now) |
| 4-digit waypoint IDs | `waypointIDPattern`, and the `waypoint.id` CHECK | A new migration changing the CHECK, and the pattern, together |
| `FitsShape` cost | linear in a shape's patterns (156 max in the seed) | Revisit the lookup-table alternative only at thousands of patterns per shape |

## Not decided yet
- **Spells: any subset, or only defined patterns?** The PR assumes a spell for every subset of 2–6 waypoints (1,474 subsets of the seed's 11). Main has 359 curated patterns of selected shapes, and only those can complete. Spells for arbitrary subsets would be a game-rule change (completing something that isn't a pattern), not just a data change.
- **Spells: what damage is applied to.** Nothing on main has HP or a target. Likely a future use case that reads `Pattern.Spell` when teamstate completes a pattern (through a small interface of its own, or a callback from `server.Build`, AGENTS.md §4).
- **Spells: rounding** of `Damage` to `int`, and how the hull treats collinear points, are the PR's to state here.
- **Coordinates aren't range-checked.** A waypoint at `0, 0` would load and, once positions are computed, skew the projection's origin. Consider a check in `New` alongside spells.
- **`Waypoint(i)` and friends panic on a bad index** rather than returning `ok`. Fine while only atlas-issued indexes are passed.

## Tests
`tests/store_test.go`. All but `TestDevSeedLoads` need no database: they call `atlas.New` on a fixture of four waypoints `1001`–`1004` (A–D): triangle `10` = ABC, line `11` = AD, triangle `12` = BCD. `game.max_pattern_points` comes from `config.toml`; below the fixture's 3 points (`fixtureMaxPoints`) these tests **skip**:

| Test | Covers |
|---|---|
| `TestPatternLookupIgnoresScanOrder` | a pattern is found from its waypoints in any of three orders; `WaypointIDs` gives them sorted; a partial, an extra and the empty set match nothing; an unknown waypoint in `SetOf` is an error |
| `TestPatternIndexes` | waypoint and shape indexes in ID order (`1004` → 3, `triangle` → 1); an unknown waypoint has none; `ShapePatterns(triangle)` = `10, 12`; `PatternByID(12)`; `WaypointIDs` sorts |
| `TestAtlasValidation` | each startup refusal, by its error: ID not 4 digits (`A1`, `10001`), unknown waypoint, unknown shape, empty pattern, two patterns with the same waypoints (across shapes, in another order), a pattern over `game.max_pattern_points`, a pattern nested in a larger one of its shape, more than 64 waypoints, 64 shapes, 2048 patterns |
| `TestBitsets` | `PatternSet` across 64-bit words (bits 0, 63, 64, 70, 2047) with `Indexes` and `Has`; `WaypointSet` `With` (twice the same), `Has`, `Len`, `Contains` |
| `TestMixedSizesAllowedWhenNotNested` | a shape may mix 2- and 3-point patterns when none is inside another |
| `TestFitsShape` | one waypoint in either triangle, a pair in both, a pair in none, a whole pattern, a waypoint on no line, a pair on a line, and the empty set → `false` |
| `TestDevSeedLoads` | real Postgres (`TEST_DATABASE_URL`, schema `atlas_test`): the 2025 seed loads through `Load` with config.toml's rules: 11 waypoints, 5 shapes, 359 patterns; square `1001 1002 1007 1008` is found as a `square`; waypoint 0 is `1001` with coordinates; seeding again succeeds (idempotent) |
| `BenchmarkLookups` | `FitsShape`, `PatternFor` and `WaypointIndex`+`Has` on the fixture (not the seed), results kept in package-level sinks (`sinkBool`, `sinkInt`). On an i7-12650H: about 1 ns, 2 ns and 6 ns |

Run `go test -bench . ./internal/atlas/` for the benchmark. Not tested here: `Load`'s `load atlas:` error path (no database fault injection). Teamstate's tests (`internal/teamstate/README.md`) exercise the atlas through `teamstate.Atlas` on the seed, including a 1-point and a 6-point pattern added by SQL.
