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
| type | | `Shape` | line, triangle, quadrilateral, …; what a team selects |
| shape | | `Pattern` | one valid set of 4-digit waypoint IDs of a shape |
| point | | `Waypoint` | a QR location, 4-digit ID |
| | spell, powerup, damage | `Spell` | what completing a pattern casts: the team's reward |
| class | | `SpellClass` | a group of patterns by waypoint count: every 4-point pattern is `class_4`, whatever its shape |

| Term | Meaning |
|---|---|
| **Waypoint** | A physical location with a QR code. Identified by a **4-digit ID** (`"2001"`; leading zeros allowed). Has a display name (`"G Block Junc"`) and WGS 84 latitude and longitude. |
| **Shape** | What a pattern looks like. In the 2026 seed one per size: `line` (2), `triangle` (3), `quadrilateral` (4), `pentagon` (5), `hexagon` (6). (2025's were drawn shapes: `collinear`, `lightning`, `square`, `tree`, `triangle`.) Identified by a lowercase slug. A shape has many patterns. |
| **Pattern** | A set of **1 to 6** (`game.max_pattern_points`) waypoints that forms a shape on the map. Identified **by that set**, in any order. Its Postgres `id` is only a key (what `team_pattern` stores). |
| **Index** | A waypoint's, shape's or pattern's position in the atlas's slices. Exists only in memory and may differ between restarts. Postgres, the API and live events always use IDs. |
| **Set** | A bitmask over indexes: `WaypointSet`, `ShapeSet`, `PatternSet`. Bit *i* means item *i*. |
| **Nested pattern** | A pattern contained in a larger pattern of the same shape. Refused at startup: scanning the larger one would pass through the smaller one first and complete it early. |
| **Spell** | What completing a pattern casts: `Spell{Damage, Class}`, one per pattern, read from Postgres once at startup (`pattern.damage`, `pattern.spell_class_id`; see [Spells](#spells)). |
| **Spell class** | A group of patterns by **how many waypoints they have**: every pattern of *n* waypoints, whatever its shape, is in the class whose `points` is *n* (`class_4` for every 4-point pattern, whatever its shape). A row of `spell_class` (migration 0012): a string `id` (`"class_4"`), a display `name` (`"Class 4"`) and its `points`. Each pattern names its class (`pattern.spell_class_id`); `Load` refuses a pattern whose class doesn't match its waypoint count. Held on `Pattern.Spell.Class` (the ID). |
| **Read-only use case** | Static content, no runtime writes, no routes. It has `doc.go`, `read.go`, `db.go`, `store.go` and this README, and no `api.go` or `write.go` (AGENTS.md §3). |

## The flow
1. **Content is written** by migrations and seeds (`go run . seed` loads the 2026 content, `seeds/2026.sql`), with the backend stopped. The atlas never writes.
2. **`server.Build` calls `atlas.Load` first**, before any use case that reads it, with `atlas.Rules{MaxPatternPoints: game.max_pattern_points}`.
3. **`Load` reads** every waypoint, shape and pattern (`db.selectAtlas`), **validates** them (`New`), and builds the lookup tables. Bad content refuses startup.
4. **`Load` computes nothing**: each pattern's spell (damage and class) is read as stored; the geometry behind the damage runs offline, in `internal/spells` (see [Spells](#spells)).
5. **`Load` logs** `msg="loaded atlas" waypoints=10 shapes=5 patterns=837` (2026 seed) and returns the `*Atlas`.
6. **Other use cases read it** through small interfaces of their own. teamstate declares `teamstate.Atlas` and uses it to validate selections and scans, label wrong scans (`FitsShapeCastable`), detect completions (`PatternFor`), rebuild stored state, and build `GET /api/game/shapes`.

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
- **Types:** `Waypoint`, `Shape`, `Pattern`, `Spell`, `SpellClass`, `Atlas` (see [Data model: memory](#data-model-memory)).
- **Counts:** `WaypointCount()`, `ShapeCount()`, `PatternCount()`. Indexes run from 0 to count − 1.
- **By index:** `Waypoint(i)`, `Shape(i)`, `Pattern(i)` return copies. An out-of-range index panics (slice indexing); callers only pass indexes the atlas gave them.
- **By ID:** `WaypointIndex(id)`, `ShapeIndex(id)`, `PatternByID(id)` (Postgres key) return `(index, ok)`.
- **`SetOf(ids)`**: IDs to a `WaypointSet`; `unknown waypoint "9999"` on the first unknown ID. Duplicates collapse (it's a set). Used by `New` and teamstate's `stateOf`.
- **`PatternFor(set)`**: the pattern made of **exactly** these waypoints, one map lookup. Teamstate's completion check.
- **`ShapePatterns(shape)`**: the shape's pattern indexes, in key order. The slice is shared: callers must not modify it. Used by `checkNesting` and teamstate's `newGame` (`FitsShapeCastable` reads `shapePatterns` directly).
- **`FitsShapeCastable(shape, set, maxPoints, cooldown)`**: whether some pattern of the shape with at most `maxPoints` waypoints and not of spell class `cooldown` (`""` leaves none out: every pattern has a class) contains every waypoint in `set` (see [Does a set still fit the shape?](#does-a-set-still-fit-the-shape)). The empty set fits nothing. Teamstate's wrong-scan check: it passes the team's unlocked size (`unlocked_points`: **4** until the team has defeated **5** bosses, `game.locked_pattern_points` − 1 and `game.unlock_after_bosses`, then **6**) and its cooldown class (its last cast's), so a set that only a locked or cooled-down pattern contains is wrong. With `maxPoints` = `game.max_pattern_points` and `""`, every pattern of the shape counts.
- **`SpellClasses()`**: every spell class, ascending by points. The slice is shared: callers must not modify it. Used by teamstate's `newGame` for `GET /api/game/shapes`.
- **`WaypointIDs(set)`**: a set back to IDs, sorted. Used for every view and `ScanResult.Pattern`, and the string key for a pattern ([Spells](#spells), identity).
- No lookup locks or touches Postgres. Only `WaypointIDs` and the `Indexes` methods allocate (their result slices).

### `store.go`: layout, `Rules`, `Load`, validation
- **Layout comment** drawing the `Atlas` (below).
- **`waypointIDPattern`** `^[0-9]{4}$`: a format, not a tunable; matches the `waypoint.id` CHECK in migration 0003.
- **`Rules`**: `MaxPatternPoints` (`game.max_pattern_points`). Damage isn't computed here: it's precomputed offline by `cmd/spells` and read from `pattern.damage` (see [Spells](#spells)).
- **`Load(ctx, pg, log, rules)`**: `selectAtlas` (error → `load atlas: …`), then `New(src, rules)` (error → `invalid atlas: …`), then the `loaded atlas` log line. Called once, by `server.Build`, and by the tests.
- **`New(src, rules)`**: builds an Atlas from a `Source`, validating in the order of [Loading at startup](#loading-at-startup) and builds every slice and map. Waypoints and shapes take their index from their row order (ID order, from the query); patterns from theirs (key order).
- **`spellClasses(src)`**: validates the classes (points at least 1; each ID and each points value at most once) and returns them sorted by points, with an index by ID. Called first by `New`.
- **`checkNesting(shape, patterns)`**: for every pair of the shape's patterns, refuses one that's smaller than and contained in another. O(n²) per shape: 252² ≈ 63,500 AND-compares for the 2026 seed's pentagons, once at startup.

### `db.go`: SQL (reads only)
- **`db{pool}`**: the pool. The atlas never writes; content changes only through migrations and seeds, with the backend stopped.
- **`Source`** (`Waypoints []Waypoint`, `Shapes []Shape`, `Patterns []PatternSource`, `SpellClasses []SpellClass`) and **`PatternSource`** (`ID`, `Shape` (the shape's ID), `Waypoints []string`, `Damage`, `Class` (the spell class's ID, `""` if none)): the content tables as stored, by IDs. Declared in `store.go`; `selectAtlas` fills one from Postgres; tests build one by hand and pass it to `New`. Exported for that reason (AGENTS.md §13).
- **`selectAtlas(ctx)`** runs three queries on the pool, not in a transaction (nothing writes content while the backend runs, and the instance lock keeps `seed` out):
  1. `SELECT id, name, latitude, longitude FROM waypoint ORDER BY id`
  2. `SELECT id, name FROM shape ORDER BY id`
  3. `SELECT id, name, points FROM spell_class ORDER BY points`
  4. `SELECT p.id, p.shape_id, COALESCE(array_agg(pw.waypoint_id ORDER BY pw.waypoint_id) FILTER (WHERE pw.waypoint_id IS NOT NULL), '{}'), COALESCE(p.damage, 0), COALESCE(p.spell_class_id, '') FROM pattern p LEFT JOIN pattern_waypoint pw ON pw.pattern_id = p.id GROUP BY p.id, p.damage, p.spell_class_id ORDER BY p.id`. The `LEFT JOIN` and `COALESCE` return a pattern with no waypoints as `{}`, so `New` can refuse it instead of it vanishing; a missing damage reads as 0 and a missing class as `""`, so `New` refuses them by name.

### `tests/store_test.go`
See [Tests](#tests).

## Data model: memory
```
Atlas                                built once by Load; never modified after
  waypoints      []Waypoint          index → waypoint, ID order
  shapes         []Shape             index → shape, ID order
  patterns       []Pattern           index → pattern, Postgres key order
  classes        []SpellClass        spell classes, ascending by points
  waypointIndex  map[string]int      "2007" → 6
  shapeIndex     map[string]int      "quadrilateral" → 3
  patternByID    map[int]int         Postgres key → index (87 → 86)
  patternBySet   map[WaypointSet]int exact set → index (195 → 187)
  shapePatterns  [][]int             shape index → its pattern indexes, key order
```

| Struct | Field | Go type | Meaning |
|---|---|---|---|
| `Waypoint` | `ID` | `string` | 4 digits, e.g. `"2001"`; identity everywhere (JSON `id`) |
| | `Name` | `string` | display, e.g. `"G Block Junc"` (JSON `name`) |
| | `Latitude` | `float64` | degrees, WGS 84, from Postgres (JSON `latitude`) |
| | `Longitude` | `float64` | degrees, WGS 84, from Postgres (JSON `longitude`) |
| `Shape` | `ID` | `string` | what a selection names, e.g. `"quadrilateral"` (JSON `id`) |
| | `Name` | `string` | display, e.g. `"Quadrilateral"` (JSON `name`) |
| `Pattern` | `ID` | `int` | Postgres `pattern.id`; what `team_pattern` stores |
| | `Shape` | `int` | its shape's index |
| | `Waypoints` | `WaypointSet` | its waypoints |
| | `Spell` | `Spell` | what completing it casts, read from `pattern.damage` and `pattern.spell_class_id` |
| `Spell` | `Damage` | `int` | what the spell deals |
| | `Class` | `string` | its spell class's ID, e.g. `"class_4"`: the class of patterns with its waypoint count (`pattern.spell_class_id`) |
| `SpellClass` | `ID` | `string` | stable identifier clients see, e.g. `"class_4"` (JSON `id`) |
| | `Name` | `string` | display, e.g. `"Class 4"` (JSON `name`) |
| | `Points` | `int` | the waypoint count of its patterns (JSON `points`) |

- **Size**, from `unsafe.Sizeof` on amd64: `Waypoint` 48 bytes, `Shape` 32, `Pattern` 48 (of which `Spell` 24). With the 2026 seed the three slices are 10 × 48 + 5 × 32 + 837 × 48 = **40,816 bytes**, plus the strings and the five maps: tens of KB. At the limits (64 waypoints, 64 shapes, 2048 patterns) the slices are about 101 KB (103,424 bytes).
- **Why slices plus maps.** Teamstate stores indexes in fixed-size bitsets, so it needs index → item (slices) and ID → index (maps). Patterns are found by set, not ID, during play, hence `patternBySet`; `patternByID` is only for loading `team_pattern`.
- **Why `Pattern` holds `Spell` by value.** A scan that completes a pattern reads `Pattern(i).Spell` as a field: no map, no computation.
- **No `json` tags on `Pattern`, `Spell`.** No route serialises them today.

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
| | `damage` | `INTEGER` | `CHECK (damage > 0)`, nullable (migration 0008) | a spell deals positive damage; `New` refuses a missing one |
| | `spell_class_id` | `TEXT` | REFERENCES `spell_class (id)`, nullable (migration 0012) | a pattern's class exists; `New` refuses a missing one, and one whose `points` isn't the pattern's waypoint count |
| `spell_class` | `id` | `TEXT` | PRIMARY KEY, `CHECK (id ~ '^[a-z0-9_-]{1,32}$')` | a string slug, unique (`"class_4"`) |
| | `name` | `TEXT` | NOT NULL | display |
| | `points` | `SMALLINT` | NOT NULL, UNIQUE, `CHECK (points BETWEEN 1 AND 64)` | one class per waypoint count |
| `pattern_waypoint` | `pattern_id` | `INTEGER` | NOT NULL, REFERENCES `pattern (id)` ON DELETE CASCADE | deleting a pattern removes its waypoint list |
| | `waypoint_id` | `TEXT` | NOT NULL, REFERENCES `waypoint (id)` | a pattern's waypoints exist (`New` checks it too) |
| | | | PRIMARY KEY `(pattern_id, waypoint_id)` | a waypoint is in a pattern at most once |

**Not constraints, checked only by `New`:** a pattern's class's `points` equals its waypoint count; a pattern has at least one waypoint, at most `game.max_pattern_points`; no two patterns share a set; no pattern is nested in a larger one of its shape; the counts fit the set types. They span rows or depend on config, which a constraint can't express.

Teamstate's tables refer to these (`team_scan.waypoint_id`, `team_pattern.pattern_id`, `team_selection.shape_id`); see `internal/teamstate/README.md`.

## Memory ↔ Postgres
| Postgres | Rebuilt in memory by `Load` |
|---|---|
| `waypoint` (ID order) | `waypoints`, `waypointIndex` |
| `shape` (ID order) | `shapes`, `shapeIndex` |
| `pattern` + `pattern_waypoint` (key order) | `patterns` (`ID`, `Shape`, `Waypoints`, `Spell.Damage`, `Spell.Class`), `patternByID`, `patternBySet`, `shapePatterns` |
| `spell_class` (points order) | `classes` |

- **Exact on every start.** Every field is either read from Postgres or a pure function of what was read and of `config.toml` (`game.max_pattern_points`, which only validates). The same rows and config give the same atlas, indexes included (the queries are ordered).
- **Not persisted, by design:** indexes and the maps. They're derived; storing them would be a second copy that can drift. Spells are persisted (`pattern.damage`, `pattern.spell_class_id`), computed offline by `cmd/spells` (see [Spells](#spells), single source of truth).
- **Changes need a restart.** Content edited while the backend runs isn't seen until the next start.

## How waypoints and patterns are stored

### Indexes
At startup every waypoint, shape and pattern gets an **index**: its position in an `Atlas` slice.
- Waypoints and shapes by ID; patterns by Postgres key.
- 2026 seed: waypoints `2001`→0 … `2010`→9; shapes `hexagon`→0, `line`→1, `pentagon`→2, `quadrilateral`→3, `triangle`→4 (ID order); patterns `1`→0 … `837`→836 (keys are 1–837 with no gaps).
- Indexes exist **only in memory**. Postgres, the API and live events always use IDs.

### Sets are bitmasks
Bit *i* means item *i*:

| Type | Go type | Holds |
|---|---|---|
| `WaypointSet` | `uint64` | up to 64 waypoints |
| `ShapeSet` | `uint64` | up to 64 shapes |
| `PatternSet` | `[32]uint64` | up to 2048 patterns |

Example: the quadrilateral `2001, 2002, 2007, 2008` (pattern `188`) is indexes 0, 1, 6, 7, so its `WaypointSet` is `0b11000011` = **195**. Any scan order sets the same bits.

### Lookup tables
| Field | Maps | Used for |
|---|---|---|
| `waypoints`, `shapes`, `patterns` | index → item | turning indexes back into IDs |
| `waypointIndex` | `"2007"` → 6 | validating a scan, `SetOf` |
| `shapeIndex` | `"quadrilateral"` → 3 | validating a selection |
| `patternBySet` | `195` → 187 (pattern `188`, the quadrilateral) | the completion check, one map lookup (`PatternFor`; about 2 ns on the test fixture, `BenchmarkLookups`) |
| `patternByID` | `87` → 86 | loading `team_pattern` (teamstate's `stateOf`) |
| `shapePatterns` | `3` → the quadrilateral's 210 pattern indexes | `FitsShapeCastable`, `checkNesting`, teamstate's `newGame` (each shape's count and sizes) |

### Bit operations
| Question | Operation |
|---|---|
| Visited 2007? | `set & (1<<6) != 0` (`Has`) |
| Stack plus this scan | `set \| 1<<w` (`With`) |
| Still inside a pattern of the shape? | any `p` of the shape with `p & set == set` (`Contains`, in `FitsShapeCastable`) |
| Stack *is* a pattern? | `patternBySet[set]` (exact, `PatternFor`) |
| How many points? | `bits.OnesCount64(set)` (`Len`) |

### Does a set still fit the shape?
`FitsShapeCastable(shape, set, maxPoints, cooldown)`: is there a pattern of the shape the team may cast now containing every waypoint in `set`? Teamstate calls it with the stack plus the new scan, the team's unlocked size and its cooldown class; `false` marks the scan **wrong**, it doesn't reject it.

1. The empty set fits nothing (`false` at once).
2. For each index `p` in `shapePatterns[shape]`: if `patterns[p]` has at most `maxPoints` waypoints, isn't of class `cooldown`, and `patterns[p].Waypoints & set == set`, return `true`.
3. None matched: `false`.

One AND-compare per pattern of the shape, with an early exit. The 2026 seed:

| Shape | Patterns | Points |
|---|---|---|
| `line` | 45 | 2 |
| `triangle` | 120 | 3 |
| `quadrilateral` | 210 | 4 |
| `pentagon` | 252 | 5 |
| `hexagon` | 210 | 6 |

**Every combination is a pattern**, so with the 2026 seed `FitsShapeCastable` with no size limit and no cooldown is `true` for any set of the 10 checkpoints no bigger than the shape's size: the first pattern checked usually contains it. A miss (and so a wrong scan) needs a waypoint in none of the shape's patterns, which the real content doesn't have (tests add one, `2099`). What still narrows a fit is the size limit (the unlock) and the class cooldown.

Measured once on the 2025 seed, by hand, before the size and class conditions were added (the committed `BenchmarkLookups` runs on the small test fixture instead): about **1 ns** on a hit (early exit), about **9 ns** on a miss (all 156 of its triangles checked). A 2026 miss scans at most 252 patterns (the pentagons).

**Why not a lookup table.** The alternative is a precomputed map from every `(shape, subset of one of its patterns)` to `true`. On the 2025 seed that was **983** entries, and, measured once there, a flat **15–19 ns**: hashing a key costs more than scanning at most 156 `uint64`s. So the loop stays. Revisit only if a shape grows to thousands of patterns.

## Spells
What completing a pattern casts: the team's reward for it. **One per pattern**, held in memory on `Pattern.Spell.Damage`, read from Postgres once at startup, so a scan that completes a pattern reads it as a plain field access.

### How it works
- **Stored in Postgres:** `pattern.damage` (INTEGER, CHECK > 0, added in migration `0008_pattern_damage.sql`).
- **Loaded once at startup:** `db.selectAtlas` selects `COALESCE(p.damage, 0)`, scanning it into `PatternSource.Damage`; a missing (`NULL`) damage reads as 0. `atlas.New` validates that `Damage > 0`, failing startup with `pattern N: damage must be positive, got 0 (run cmd/spells, then go run . seed, …)`, and stores it in `Pattern.Spell.Damage`.
- **After migration 0008, reseed.** The migration adds the column empty, so an existing database refuses to start until `go run . seed` (backend stopped) fills it from the spells seed (`seeds/2026_spells.sql` now). Tested by `TestLoadRefusesMissingDamage`.
- **Zero runtime or startup computation:** The backend never runs geometric algorithms, tours, or formula math while serving or loading.
- **Offline math and generator:** All geometric modeling (equirectangular projection around waypoints' mean, Traveling Salesperson closed loop tour, Andrew's monotone chain convex hull, and damage calibration) lives in `internal/spells/`. The offline CLI `cmd/spells` reads waypoints and patterns from Postgres (`DATABASE_URL`, required: seed first) and generates `seeds/2026_spells.sql`.
- **Seed execution:** `Seed2026` runs `seeds/2026.sql` (content), then `seeds/2026_spells.sql` (damage, generated by `cmd/spells`), then `seeds/2026_bosses.sql`, so a freshly seeded database has damage populated. The 2026 damages equal `damage_lookup.json`'s for every pattern (`TestSeedMatchesDamageLookup`, spells tests).

### The in-memory struct
| Where | What | Meaning |
|---|---|---|
| `Pattern.Spell` | `Spell{Damage int, Class string}` | The pattern's damage, from `pattern.damage`, and its spell class ID, from `pattern.spell_class_id`. |

### Spell classes
- **A class is a waypoint count.** Every pattern of *n* waypoints is in the class whose `points` is *n*, whatever its shape: `class_4` holds every 4-point pattern (in 2026, every quadrilateral; in 2025 squares, lightnings and trees alike). It's stored, not computed, so the content loaded before the event says it explicitly and SQL can group by it; `New` refuses a pattern whose class's `points` isn't its waypoint count, so the stored copy can't drift.
- **A string ID, in its own table** (`spell_class`, migration 0012), like `shape`: Postgres rejects an unknown class through the foreign key, and a class can gain fields (a display name today; a cooldown rule or an icon later) without touching the pattern rows.
- **Seeded** by `seeds/2026.sql` (which `Seed2026` runs first): `class_1` to `class_15` (one per size `game.max_pattern_points` allows), and every pattern inserted with `class_n` for its *n* waypoints.
- **After migration 0012, reseed.** The column is added empty, so an existing database refuses to start (`pattern N has no spell class`) until `go run . seed` (backend stopped) fills it. Tested by `TestLoadRefusesMissingSpellClass`.
- **Shown to clients** as `class` on every cast (`last_cast`, `GET /api/teams/me/casts`), and in `GET /api/game/shapes` as each shape's `classes` (one per size in `points`) and the full `spell_classes` list (teamstate's README, "API").

### Identity
- Waypoints by **4-digit ID** (`2001`–`2010`); patterns by **`WaypointSet`** (in memory) or Postgres **`ID`**.
- **Never short names** like `"CnD"`: they aren't stored in pattern definitions. They only appear inside display names (`"CnD Atrium (CnD)"`).

### Single source of truth
- Coordinates come **only from Postgres** (`waypoint.latitude`, `waypoint.longitude`).
- Damage values come **only from Postgres** (`pattern.damage`) at runtime.
- The one other file is **`damage_lookup.json`** (`internal/spells/testdata/`): the 2026 damage table, every combination of the 10 2026 checkpoints. Nothing reads it at runtime. It's the reference `config.toml`'s `[spell]` values are tuned to, and `TestDamageLookupMatchesConfig` fails if the two disagree.

### Config
The generator's coefficients live in `config.toml` under `[spell]` (the formula: [`internal/spells/README.md`](../spells/README.md)):
- `spell.base` (**1: 15, 2: 22, 3: 58, 4: 186, 5: 384, 6: 1200**): base damage by waypoint count. The biggest size (`game.max_pattern_points`, **6**) deals its base flat.
- `spell.bonus.concave_quad` (**75**): added for a 4-point pattern whose convex hull has 3 corners.
- `spell.per_100m` (**0.8**): damage per 100 m of walking distance.
- `spell.walkability` (**1.5**): straight-line to walking distance ratio.

### Drift prevention
The test `TestPatternDamageDrift` in `internal/spells/tests/` recomputes every pattern's damage from database waypoints and coefficients and verifies it matches `pattern.damage`. Any change to coordinates or coefficients without re-running `go run ./cmd/spells` fails the test.

Open questions are under [Not decided yet](#not-decided-yet).

## Walkthroughs
2026 seed, `game.max_pattern_points` = **6**.

### 1. Loading the seed
| Step | Input | Result |
|---|---|---|
| `selectAtlas` | 10 waypoint rows `2001`…`2010`, 5 shape rows, 837 pattern rows | `Source` |
| counts | 10 ≤ 64, 5 ≤ 64, 837 ≤ 2048 | ok |
| waypoints | `2001` matches `^[0-9]{4}$` … | `waypointIndex["2001"] = 0` … `["2010"] = 9` |
| shapes | `hexagon` … `triangle` (ID order) | `shapeIndex["quadrilateral"] = 3` |
| pattern row `{188, "quadrilateral", ["2001","2002","2007","2008"]}` | shape known; `SetOf` → 195; 4 ≤ 6; 195 not seen | `patterns[187] = {ID 188, Shape 3, Waypoints 195}`; `patternByID[188] = 187`; `patternBySet[195] = 187`; `shapePatterns[3]` += 187 |
| pattern row `{50, "triangle", ["2001","2002","2007"]}` | `SetOf` → 67 | `patternBySet[67] = 49`; `shapePatterns[4]` += 49 |
| `checkNesting` | 67 ⊂ 195, but triangle ≠ quadrilateral | ok: nesting is only refused within one shape (and every 2026 shape is one size, so none of its patterns can nest) |
| log | | `msg="loaded atlas" waypoints=10 shapes=5 patterns=837` |

### 2. Lookups during a quadrilateral (teamstate's calls)
Team selected `quadrilateral` (shape 3):

| Scan | Set | `FitsShapeCastable(3, set, 6, "")` | `PatternFor(set)` | Teamstate does |
|---|---|---|---|---|
| `2001` | 1 | true | none | `scan_added` |
| `2002` | 3 | true | 0, a **line** (shape 1 ≠ 3) | `scan_added`: wrong shape, no completion |
| `2007` | 67 | true (inside 195) | 49, a **triangle** (shape 4 ≠ 3) | `scan_added`: wrong shape, no completion |
| `2008` | 195 | true | 187, shape 3 | `pattern_completed`, `WaypointIDs(195)` = `["2001","2002","2007","2008"]`; `team_pattern` stores `Pattern(187).ID` = 188 |

Any third scan would have fit: every 4 of the 10 checkpoints is a quadrilateral, so every set of up to 4 is inside one. Only a waypoint in no pattern (a test's `2099`), a locked size or the cooldown class makes `FitsShapeCastable` `false` and teamstate mark the scan wrong. (2025's drawn shapes were different: there, `1001 1002 1003` was in no square, so that third scan was wrong.)

### 3. A refused start
A migration adds a shape `zigzag` with pattern `900`, waypoints `2001 2002 2007` (set 67), and pattern `901`, waypoints `2001 2002 2007 2008` (set 195): two patterns of one shape, one inside the other. But both sets are already 2026 patterns (`50` and `188`), so the first refusal is the duplicate:

```
level=ERROR msg=fatal error="invalid atlas: patterns 50 and 900 use the same waypoints"
```
(`main` logs it with the `slog` text handler, after the `time=` field, and exits `1`.)
With fresh sets (through a waypoint in no 2026 pattern) the nesting check refuses instead: `shape "zigzag": pattern 900 is inside pattern 901, so it would complete first`. A team scanning 901 would hit 900 a scan early and complete it instead. Fix the content, restart.

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
| 4a | A spell class with points below 1, an ID twice, or two classes for one waypoint count | `invalid atlas: spell class "none": points must be at least 1, got 0` / `spell class "class_3" appears twice` / `spell classes "class_3" and "trio" are both for 3 waypoints` |
| 5 | A waypoint ID isn't exactly 4 digits (first in ID order) | `invalid atlas: waypoint id "A1" is not 4 digits` |
| 6 | A pattern names an unknown shape (patterns in key order; 6–10 are per pattern) | `invalid atlas: pattern 10: unknown shape "hexagon"` |
| 7 | A pattern names an unknown waypoint | `invalid atlas: pattern 10: unknown waypoint "9999"` |
| 8 | A pattern has no waypoints | `invalid atlas: pattern 10 has no waypoints` |
| 9 | A pattern has more than 6 (`game.max_pattern_points`) waypoints | `invalid atlas: pattern 98 has 7 waypoints; game.max_pattern_points is 6` |
| 10 | A pattern's damage is missing or not positive | `invalid atlas: pattern 7: damage must be positive, got 0 (run cmd/spells, then go run . seed, …)` |
| 11 | A pattern has no spell class | `invalid atlas: pattern 7 has no spell class (run go run . seed, with the backend stopped)` |
| 12 | A pattern names an unknown spell class | `invalid atlas: pattern 10: unknown spell class "class_99"` |
| 13 | A pattern's class is for another waypoint count | `invalid atlas: pattern 10 has 3 waypoints but spell class "class_4" is for 4` |
| 14 | Two patterns have the same set of waypoints (in any order, any shapes) | `invalid atlas: patterns 10 and 99 use the same waypoints` |
| 15 | A pattern is inside a larger pattern of the same shape (shapes in index order) | `invalid atlas: shape "triangle": pattern 97 is inside pattern 10, so it would complete first` |

**Allowed, at the boundaries:**

| Case | Result |
|---|---|
| Exactly 64 waypoints, 64 shapes, 2048 patterns | Loads |
| A pattern of exactly `game.max_pattern_points` waypoints | Loads |
| A 1-point pattern | Loads (unless it's inside a larger pattern of its shape) |
| One shape mixing sizes (2- and 3-point patterns), none inside another | Loads |
| A pattern inside a larger pattern of **another** shape (the seed's triangle 67, `2001 2002 2007`, inside quadrilateral 195) | Loads: only the selected shape can complete |
| A shape with no patterns | Loads; `GET /api/game/shapes` shows it with `patterns: 0` |
| A waypoint in no pattern | Loads; scanning it is always a wrong scan (the only way to get one with 2026-style content, where every combination is a pattern; tests add `2099`) |
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
| `FitsShapeCastable(shape, set, max, cooldown)` | Empty set | `false` |
| | Inside at least one of the shape's patterns of at most `max` waypoints, not of class `cooldown` | `true` |
| | Spans two patterns, or touches none | `false` |
| | Only inside bigger patterns | `false` |
| | Only inside patterns of class `cooldown` (an exact cooled-down pattern included, so it can't complete) | `false` |
| | `max` at least the shape's biggest pattern | the same as no size limit |
| | `cooldown` `""` or an unknown class | excludes nothing |
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
| `FitsShapeCastable` cost | linear in a shape's patterns (252 max in the 2026 seed, the pentagons) | Revisit the lookup-table alternative only at thousands of patterns per shape |

## Not decided yet
- **Spells: any subset, or only defined patterns?** Decided for 2026: every subset of 2–6 of the 10 checkpoints is a pattern (837), so a spell exists for every combination (`damage_lookup.json`, `seeds/2026.sql`). 2025 had 359 curated patterns of drawn shapes. The game rule is unchanged (only patterns complete); the content makes every combination one, which is why the real content has no wrong scans.
- **Spells: what damage is applied to.** Decided: teamstate deals each cast's `Spell.Damage` (× the team's multiplier) to the team's current boss ([`internal/bosses/README.md`](../bosses/README.md), "Damage rules").
- **Coordinates aren't range-checked.** A waypoint at `0, 0` would load, and `cmd/spells` would project it into the damage it computes offline (skewing the projection's origin). Consider a check in `New`.
- **`Waypoint(i)` and friends panic on a bad index** rather than returning `ok`. Fine while only atlas-issued indexes are passed.

## Tests
`tests/store_test.go`. All but `TestSeedLoads` and the two `TestLoadRefuses…` tests need no database: they call `atlas.New` on a fixture of four waypoints `1001`–`1004` (A–D): triangle `10` = ABC, line `11` = AD, triangle `12` = BCD, each in the class of its size (`classOf`), with spell classes `class_1` to `class_15` (`fixtureClasses`, in reverse order). `game.max_pattern_points` comes from `config.toml`; below the fixture's 3 points (`fixtureMaxPoints`) these tests **skip**:

| Test | Covers |
|---|---|
| `TestPatternLookupIgnoresScanOrder` | a pattern is found from its waypoints in any of three orders; `WaypointIDs` gives them sorted; a partial, an extra and the empty set match nothing; an unknown waypoint in `SetOf` is an error |
| `TestPatternIndexes` | waypoint and shape indexes in ID order (`1004` → 3, `triangle` → 1); an unknown waypoint has none; `ShapePatterns(triangle)` = `10, 12`; `PatternByID(12)`; `WaypointIDs` sorts |
| `TestAtlasValidation` | each startup refusal, by its error: ID not 4 digits (`A1`, `10001`), unknown waypoint, unknown shape, empty pattern, zero and negative damage, no spell class, an unknown class, a class for another size, a class ID twice, two classes for one size, a class of 0 points, two patterns with the same waypoints (across shapes, in another order), a pattern over `game.max_pattern_points`, a pattern nested in a larger one of its shape, more than 64 waypoints, 64 shapes, 2048 patterns |
| `TestSpellClasses` | every fixture pattern carries `class_n` for its *n* waypoints; `SpellClasses` lists the 15 classes ascending by points though the fixture gives them in reverse |
| `TestBitsets` | `PatternSet` across 64-bit words (bits 0, 63, 64, 70, 2047) with `Indexes` and `Has`; `WaypointSet` `With` (twice the same), `Has`, `Len`, `Contains` |
| `TestMixedSizesAllowedWhenNotNested` | a shape may mix 2- and 3-point patterns when none is inside another |
| `TestFitsShapeAll` | `FitsShapeCastable` with no size limit (`game.max_pattern_points`) and no cooldown: one waypoint in either triangle, a pair in both, a pair in none, a whole pattern, a waypoint on no line, a pair on a line, and the empty set → `false` |
| `TestFitsShapeSizeLimit` | `FitsShapeCastable` with no cooldown, on a line shape with a 2- and a 3-point pattern: a point only in the 3-point one fits within 3, not within 2; a limit below every pattern fits nothing; a pair spanning both lines doesn't fit; the empty set doesn't; and a limit at least the biggest pattern equals no limit |
| `TestFitsShapeCastable` | a line shape with a 2-point (`class_2`) and a 3-point (`class_3`) pattern: a point only in the cooled-down class doesn't fit, one in the other class does, the size limit still applies on top, the exact cooled-down pattern doesn't fit (so it can't complete), an unknown class excludes nothing, the empty set fits nothing; no cooldown (`""`) gives the same as an unknown class |
| `TestSeedLoads` | real Postgres (`TEST_DATABASE_URL`, schema `atlas_test`): the 2026 seed loads through `Load` with config.toml's rules: 10 waypoints, 5 shapes, 837 patterns; each shape holds exactly the combinations of its size (45 lines, 120 triangles, 210 quadrilaterals, 252 pentagons, 210 hexagons), each pattern with positive damage and `class_n`; `2001 2002 2003 2004` is a `quadrilateral`; waypoint 0 is `2001`, `G Block Junc`, with coordinates; seeding again succeeds and still loads 837 (idempotent) |
| `TestLoadRefusesMissingDamage` | real Postgres: a seeded pattern's damage set to `NULL` (migration 0008 without the spells seed) makes `Load` fail with `pattern 7: damage must be positive`, not a scan error |
| `TestLoadRefusesMissingSpellClass` | real Postgres: a seeded pattern's `spell_class_id` set to `NULL` (migration 0012 without the seed) makes `Load` fail with `pattern 7 has no spell class` |
| `BenchmarkLookups` | `FitsShapeCastable` (no size limit, no cooldown), `PatternFor` and `WaypointIndex`+`Has` on the fixture (not the seed), results kept in package-level sinks (`sinkBool`, `sinkInt`). On an i7-12650H: about 2 ns, 2 ns and 7 ns |

Run `go test -bench . ./internal/atlas/` for the benchmark. Not tested here: `Load`'s `load atlas:` error path (no database fault injection). Teamstate's tests (`internal/teamstate/README.md`) exercise the atlas through `teamstate.Atlas` on the 2026 seed (its hexagons included), with a 1-point pattern and a waypoint in no pattern (`2099`) added by SQL.
