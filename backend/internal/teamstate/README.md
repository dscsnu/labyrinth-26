# teamstate

What each team does in the game: the shape it has selected, its stack of scans toward a pattern of that shape, the waypoints it has visited, the patterns and shapes it has completed, its cast log, its progress through the bosses, and its health under their attacks. There is **no time limit** on an attempt. The one change caused by time passing is a **boss attack**: every boss attacks on a clock, lowering the team's health, published by the server with nobody acting.

Every number below that comes from `config.toml` is shown with its current value and its key, e.g. **6** (`game.max_pattern_points`). Change the file, not the code.

## Contents
1. [Concepts](#concepts)
2. [The game flow](#the-game-flow)
3. [How each file works](#how-each-file-works)
4. [Data model: memory](#data-model-memory)
5. [Data model: Postgres](#data-model-postgres)
6. [Memory ↔ Postgres](#memory--postgres)
7. [Wrong scans and poisoned attempts](#wrong-scans-and-poisoned-attempts)
8. [Bosses](#bosses)
9. [Boss attacks and health](#boss-attacks-and-health)
10. [Walkthroughs](#walkthroughs)
11. [The write-through](#the-write-through)
12. [API](#api)
13. [Rules and cases](#rules-and-cases)
14. [Concurrency and locking](#concurrency-and-locking)
15. [Failure and recovery](#failure-and-recovery)
16. [Limits and how to change them](#limits-and-how-to-change-them)
17. [Not decided yet](#not-decided-yet)
18. [Tests](#tests)

The waypoints, shapes and patterns themselves are static content owned by the **atlas** use case: how they're loaded, validated and stored (indexes, bitmasks, `FitsShape`) is in [`internal/atlas/README.md`](../atlas/README.md). The bosses (name, HP, asset, attack interval and damage) are static content owned by the **bosses** use case ([`internal/bosses/README.md`](../bosses/README.md)). This README covers what teams do with them: a team's progress through the bosses, and its health, are part of its `State`.

## Concepts
| Term | Meaning |
|---|---|
| **Atlas** | The static content (waypoints, shapes, patterns), loaded once at startup by the atlas use case and read here through the [`Atlas` interface](#storego-the-in-memory-layout). Never changes while the backend runs. |
| **Waypoint** | A physical location with a QR code. Identified by a **4-digit ID** (`"1001"`; leading zeros allowed). |
| **QR token** | The signed, rotating string a waypoint's QR shows (`"<waypoint>:<unix seconds>.<signature>"`). A scan submits it instead of the raw ID. Owned by the [`qr`](../qr/README.md) use case; verified here through the `QR` interface before the waypoint is resolved. |
| **Pattern** | A set of **1 to 6** (`game.max_pattern_points`) waypoints that forms a shape on the map. Identified **by that set**, e.g. `["1001","1002","1007","1008"]`, in any order. Its database `id` is only an internal key. |
| **Shape** | What a pattern looks like: `square`, `triangle`, `collinear`, `tree`, `lightning` in the 2025 seed. A shape has many patterns. Completing any one completes the shape. |
| **Selection** | The shape a team is currently attempting. At most one per team. |
| **Attempt** | Everything between a selection and the next selection, clear or cast. Its scans are the stack. |
| **Stack** | The attempt's accepted scans, oldest at the **bottom**, newest on **top**. It holds **one scan per waypoint**; any member may scan any number of them. Its correct scans always fit inside one pattern of the selected shape, so there are at most **6** (`game.max_pattern_points`) of them, plus at most **one wrong scan**, always on top. Capacity: **7** (`game.max_pattern_points` + 1). Scans never leave on their own: only a selection, clear or cast empties it. |
| **Wrong scan** | A scan whose waypoint, together with the scans already in the stack, isn't part of any pattern of the selected shape that the team has unlocked (`!FitsShapeWithin(shape, set, unlocked points)`). It's **pushed** (not rejected), marked `wrong`, and the player is **told**: outcome `wrong_scan`, and `stack[].wrong: true` on every phone. |
| **Poisoned** | The stack holds a wrong scan (`State.Poisoned()`, `poisoned: true` in JSON). The attempt can't cast, and every further scan is refused with `409 selection_poisoned` until someone selects again (which purges the stack). See [Wrong scans and poisoned attempts](#wrong-scans-and-poisoned-attempts). |
| **Cast** | A completion: the moment the stack becomes exactly a pattern of the selected shape. The client calls it casting a spell. Every cast is logged, repeats included: in memory (`State.Casts`) and in `team_cast`. `team_pattern` keeps only the first completion of each pattern. |
| **Cast log** | `State.Casts`: every cast the team has made, oldest first, loaded from `team_cast` at startup. `GET /api/teams/me/casts` returns it newest first. |
| **Version** | `State.Version`, `version` in JSON (`live.Version`, `{epoch, seq}`): orders the team's snapshots so a client keeps the newest copy, from the stream or a response. Every apply sets the next one; memory-only. |
| **Cast count** | `cast_count` in JSON: `len(State.Casts)`. A client holding the log re-fetches it when this is more than it has. |
| **Last cast** | `State.LastCast()`, `last_cast` in JSON: the cast log's newest entry (pattern, shape, who, when, whether the pattern was already completed, damage, and what it did to a boss: `dealt`, `boss`, `defeated_boss`). Kept until the next cast; a new selection or a clear doesn't remove it. |
| **Spell lifecycle** | What the client shows as a spell, read from `team_state` alone, with no event of its own: it **starts** when `selection` is set, members **add their points** as `stack` grows (`stack[].user_id` says who scanned each), it **fails** when `poisoned` turns true, and it **finishes** when `last_cast` changes (a new `cast_at`). |
| **Bosses** | The static sequence every team fights, in serial order: **6** (`game.bosses`), read through the [`Bosses` interface](#storego-the-in-memory-layout). See [`internal/bosses/README.md`](../bosses/README.md). |
| **Boss progress** | `State.Boss` (`BossProgress`): `Defeated` (bosses defeated, so the current boss is at index `Defeated`), `Damage` (dealt to the current boss so far), `Multiplier`, and the phase (`PhaseStart`, `PhaseHealth`) with its derived `Attacks` and `Health`. In JSON: `bosses_defeated`, `boss: {serial, damage, phase_started_at, attacks, next_attack_at}` (`null` once every boss is defeated), `damage_multiplier`, `health`, `knocked_out`. Stored in `team_boss` (all but `Attacks` and `Health`). |
| **Dealt** | What a cast does to the current boss: `round(pattern damage × Multiplier)`. Reaching the boss's HP defeats it; the excess is lost. 0 once every boss is defeated. See [Bosses](#bosses). |
| **Multiplier** | Per team, default **1** (`team_boss.multiplier`). Nothing changes it yet. |
| **Phase** | The span a team spends on its current boss. It starts at `PhaseStart` with `PhaseHealth`: boss 1 at the team's start (`team.started_at`) with full health, boss n+1 at the cast that defeated boss n with the health then, or an admin's revive with full health. Stored in `team_boss.phase_started_at`, `phase_health` (`NULL`: the team's start, full health). |
| **Attack** | Every **10 min** (`game.boss_attack.interval`) of the phase, the current boss takes its damage (`game.boss_attack.damage`, by serial: its serial number for now) off the team's health. The k-th is at exactly `PhaseStart + k × interval`. See [Boss attacks and health](#boss-attacks-and-health). |
| **Health** | Per team, **10** (`game.health`) at the start. **Derived, never stored per attack:** `Attacks = min(floor((now − PhaseStart) / interval), ceil(PhaseHealth / attack_damage))`, `Health = max(0, PhaseHealth − Attacks × attack_damage)`. `health` in JSON. |
| **Knocked out** | Health 0 (`BossProgress.KnockedOut()`, `knocked_out` in JSON): selecting and scanning are refused (`409 knocked_out`), clearing still works, and attacks stop, until an admin revives the team (`POST /api/admin/teams/{team_id}/revive`). |
| **Catch-up** | `catchUp`: applies any attack that has fallen due, as its own change. The attack timer calls it, and so does every write before validating, so an attack is never lost to a late timer. |
| **Visited waypoints** | Every waypoint the team has ever had a scan accepted at, under any selection, wrong scans included. |
| **Clock** | Where the store gets time: `Now()` (the time selections, scans and casts are stamped with, and what health is derived against) and `AfterFunc(d, f)` (the boss attack timers). `systemClock` is the real one (`time.Now`, `time.AfterFunc`); tests pass a fake through `Rules.Clock`, whose timers fire only when the test moves its time. |
| **Game rules** | What `GET /api/game/shapes` returns: the selectable shapes (the client's "types") with their pattern counts, counts by size (`patterns_by_points`), sizes, spell classes (`classes`) and damage ranges, every spell class (`spell_classes`), plus `health` (starting health), `locked_pattern_points`, `unlock_after_bosses` and `max_pattern_points`. Static while the backend runs. |
| **Leader** | The team's leader (teams' `Team.LeaderID`). **Only the leader selects a shape and clears the selection** (`403 not_leader` for anyone else), so only the leader restarts a poisoned attempt; every member scans. Checked under the entry lock against teams' current roster (`isLeader`): an admin removing the leader names a new one. |
| **Big pattern** | A pattern of at least **5** (`game.locked_pattern_points`) waypoints. **Locked** until the team has defeated **5** (`game.unlock_after_bosses`) bosses: it can't be cast, a shape made only of big patterns can't be selected (`409 shape_locked`), and in a shape mixing sizes a scan that could only grow into a big pattern is a wrong scan. The 2025 seed has none (all its patterns have 3 or 4 points), so this only matters for content that does. |
| **Unlocked points** | `unlockedPoints(State)`, `unlocked_points` in JSON: the most waypoints a pattern the team can cast may have. **4** (`game.locked_pattern_points` − 1) until it has defeated `game.unlock_after_bosses` bosses, then **6** (`game.max_pattern_points`). `locked_shapes` lists the shapes whose every pattern is bigger. |

**Client words.** The client's **"type"** is our **Shape**, its **"shape"** is our **Pattern**, its **"point"** is our **Waypoint**, and its **"buffer"** is our **Stack**. This README and the code always use the code's words. The full vocabulary, spells included, is in [`internal/atlas/README.md`](../atlas/README.md).

## The game flow
0. **Start.** When the leader starts the team (teams' `Start`; any size from 1 to **6** (`teams.size`) may start, once every non-leader is ready), every member's stream gets the `IN_GAME` `team` and then the team's `team_state`, empty: no selection, no stack, no progress, `poisoned: false`, health **10** (`game.health`), and boss 1's attack clock started at the team's start. teams calls `PublishTeam` through its `Rules.OnStart` hook, wired in `server.Build`, because teams may not import teamstate (AGENTS.md §4). The play screen has its state at once, without reconnecting.
1. **Select.** **The leader** selects a shape: `PUT /api/teams/me/state/selection {"shape":"square"}` (anyone else: `403 not_leader`). **Every selection purges the stack**, reselecting the same shape included. Reselecting is how a team restarts an attempt. A shape whose patterns are all bigger than the team's unlocked points is refused (`409 shape_locked`) until it has defeated **5** (`game.unlock_after_bosses`) bosses.
2. **Scan.** Members scan waypoints: `POST /api/teams/me/state/scans {"qr":"1001:1760000000.…"}`. The token is verified first (signature and freshness, through the `qr` use case); then the waypoint it names is used exactly as the raw ID was. No time limit, and any member may scan any number of the pattern's waypoints (one player can scan a whole pattern). A scan is **rejected with an error and not pushed** if:
   - the token is malformed or tampered: `400 invalid_qr`
   - the token is outside its validity window: `409 qr_expired`
   - no shape is selected: `409 no_shape_selected`
   - no shape is selected: `409 no_shape_selected`
   - the attempt is **poisoned**: `409 selection_poisoned`
   - its **waypoint is already in the stack**, whoever scanned it, the same member included: `409 waypoint_already_scanned`

   Otherwise it's pushed on top. If the waypoint, with the stack, can't be part of any pattern of the selected shape **that the team has unlocked**, it's pushed **marked wrong**, the response says `wrong_scan`, and the attempt is poisoned.
3. **Complete (cast).** The moment the stack is **exactly** one of the selected shape's patterns, the pattern and its shape are marked complete, the cast is appended to the cast log (`State.Casts`, `team_cast`) and becomes `last_cast`, its damage (× the team's multiplier) goes to the current **boss** (a defeat moves the team to the next; see [Bosses](#bosses)), and **the stack and the selection are purged**. The team selects again for its next attempt.
4. **Restart.** After a wrong scan, the leader reselects (the same shape or another): the stack, wrong scan included, is purged and the team scans again.
5. **Give up.** The leader's `DELETE /api/teams/me/state/selection` clears the selection and the stack.
6. **The boss attacks** every **10 min** (`game.boss_attack.interval`) of the phase, for its `game.boss_attack.damage`, with nobody acting: the server lowers health and publishes `boss_attacked` (`knocked_out` when health reaches 0). Defeating the boss starts the next boss's clock. A knocked-out team can't select or scan until an admin revives it. See [Boss attacks and health](#boss-attacks-and-health).

**"Part of a pattern" narrows as the team scans.** The team selects a shape, not a pattern, so the first scan is correct if it's in *any* pattern of the shape. Each later scan must keep the stack inside at least one pattern of the shape that contains everything already scanned. A scan that doesn't is wrong.

**Why the shape is selected first.** With the 2025 data any three waypoints form a triangle or a collinear pattern, so a team scanning a square passes through a "triangle" after its third scan. Only the selected shape can complete, which is what makes 4-waypoint shapes possible.

**Why a wrong scan is pushed, not rejected.** The scan really happened: someone walked to that waypoint. Recording it (in the stack, in `team_scan`, in visited `waypoints`) keeps the audit trail honest, and showing it marked on every phone tells the whole team *which* point broke the attempt and who scanned it.

**Why a wrong scan poisons instead of being dropped.** If the server silently dropped the wrong scan, a team could probe which waypoints belong to the selected shape for free, one scan at a time, with no cost. Poisoning makes a mistake cost a restart: every correct scan of the attempt is lost too.

**Why no time limit.** The game used to give a pattern's scans 1 minute (plus grace) bottom to top. That's gone: an attempt lasts until it casts, is restarted or is cleared, so there are no stale scans. The pressure of time comes from the bosses instead: a slow team loses health, not scans.

**Why only the leader selects.** A selection purges the stack, so any member selecting could wipe teammates' scans mid-attempt, by accident or in a race. One person choosing the type (and deciding when to restart or give up) makes the team's plan unambiguous; scanning stays open to everyone.

**Why big patterns are locked, and why as "they don't exist yet".** 5- and 6-point patterns are the hardest and deal the most damage, so they're a reward for getting through boss 5. Treating a locked pattern as absent (the fit check counts only unlocked patterns) keeps one rule for everything: a scan that can only lead to a locked pattern is simply wrong, so a team can't half-build one and wait.

**Retries.** A phone resending a scan gets `409 waypoint_already_scanned` (or `selection_poisoned`, if its first copy was wrong); the app should treat that as "already in" and refresh from the returned state or the live event.

## How each file works

### `api.go`: routes
- **`Routes`** registers six routes behind the auth middleware (see [API](#api)); **`AdminRoutes`** registers `POST /api/admin/teams/{team_id}/revive` behind `server.Build`'s `admin` wrapper (auth, then `RequireAdmin`, which reads the role from Postgres).
- **Handlers:**
  - `shapes` returns `Store.Game()` to any signed-in user, in a team or not.
  - `getMine` returns the caller's team state (`Store.ForUser`).
  - `casts` returns the caller's team's cast log, newest first (`Store.CastsForUser`).
  - `selectShape` decodes `{"shape"}` and calls `Store.Select` (leader only).
  - `clearSelection` calls `Store.ClearSelection` (leader only).
  - `scan` decodes `{"qr"}` and calls `Store.Scan`, returning a `ScanResult`.
  - `revive` takes `{team_id}` from the path, calls `Store.Revive`, and logs `admin revived team` with the admin's ID. A malformed ID isn't checked separately: it's just not found (`404 team_not_found`).
- **`fail`** maps errors to statuses and codes via the `errorCodes` table; anything unmapped is logged and becomes `500`.

### `read.go`: types and reads
- **Types:**
  - `State`: the in-memory per-team snapshot, made of `Selection`, `Stack`, `StackScan` and `[]Cast` (see [Data model: memory](#data-model-memory)).
  - `State.Poisoned()`: true if any scan in the stack is `Wrong`.
  - `State.LastCast()`: the last element of `Casts`; `ok` is false if the log is empty.
  - `Cast`: one completed pattern (`Pattern`, `By`, `At`, `AlreadyCompleted`), and what it did to the bosses (`Dealt`, `Boss` = the serial hit or 0, `DefeatedBoss`).
  - `BossProgress`: a team's progress (`Defeated`, `Damage`, `Multiplier`) and its phase (`PhaseStart`, `PhaseHealth`, and the derived `Attacks`, `Health`); a value inside `State`.
  - `BossProgress.KnockedOut()`: `Health <= 0`.
  - `BossProgress.at(now, bosses)`: the progress with `Attacks` and `Health` as of `now`: `Attacks = floor((now − PhaseStart) / interval)` (0 if `now` is before `PhaseStart`), capped at the knockout attack `ceil(PhaseHealth / attack_damage)` (no cap when `attack_damage` is 0), and `Health = max(0, PhaseHealth − Attacks × attack_damage)`. Returns the progress unchanged if every boss is defeated. A pure function of the stored phase and the time, so the timer, a write and `Load` all get the same answer for the same time, however they got there: the cap is what makes a knocked-out team's `Attacks` the same live (where attacks stopped at the knockout) and after a restart (where `Load` computes it fresh).
  - `BossProgress.nextAttack(bosses)`: `PhaseStart + (Attacks + 1) × interval`; `ok` false if every boss is defeated or the team is knocked out.
  - `deal(progress, damage, now, bosses)`: applies one cast: returns the new progress, the damage dealt (`math.Round(damage × Multiplier)`), the serial of the boss hit, and whether it was defeated. If every boss is already defeated it returns the progress unchanged, 0, 0, false. On a defeat it increments `Defeated`, resets `Damage` to 0 (the rest of the cast is lost), and **starts the next boss's phase**: `PhaseStart` = `now`, `PhaseHealth` = the current `Health`, `Attacks` = 0. The progress passed in must already be at `now` (`Scan` caught up first). Called by `Scan`.
  - `View`, `SelectionView`, `CastView`, `BossView`, `StackScanView`: the JSON shapes. `CastView` is `last_cast` and each entry of the cast log: `pattern` (sorted waypoint IDs), `shape`, `class` (the pattern's spell class ID, e.g. `class_4`), `cast_by`, `cast_at`, `already_completed`, `damage`, `dealt`, `boss` (serial, or `null`), `defeated_boss`. `StackScanView` carries `wrong`; `View` carries `poisoned`, `bosses_defeated`, `boss` (`BossView{serial, damage, phase_started_at, attacks, next_attack_at}`, or `null`), `damage_multiplier`, `health`, `knocked_out`, `unlocked_points` and `locked_shapes`.
  - `Game`, `GameShape`, `DamageRange`: the JSON of `GET /api/game/shapes`, including each shape's `damage` range (`{"min": …, "max": …}`), `patterns_by_points` (its patterns counted by size) and `classes` (the spell class of each size, in `points` order), the game's `spell_classes` (every class, ascending by points), and the game's `health` (`game.health`), `locked_pattern_points` and `unlock_after_bosses`.
  - `Store.unlockedPoints(State)`: `Rules.MaxPatternPoints` once `State.Boss.Defeated` ≥ `Rules.UnlockAfterBosses`, else `Rules.LockedPatternPoints` − 1. Used by `view`, `Select` and `Scan`.
  - `Store.shapeLocked(shape, unlocked)`: true if the shape's smallest pattern (`Game.Shapes[shape].Points[0]`, the sizes being ascending) is bigger than `unlocked`. A shape with no patterns isn't locked.
- **`Stack` methods** (all on values; none modifies the receiver):
  - `NewStack(maxPoints)` is an empty stack with `Cap` = `maxPoints` + 1 (exported so tests can build one).
  - `Len()` counts scans.
  - `At(i)` is the i-th from the bottom (`At(0)` the oldest).
  - `Push(sc)` returns the stack with `sc` on top. It **panics** on a full stack: `Scan` never pushes onto one (correct scans fit one pattern, and nothing is pushed after a wrong scan), so a full push would be a bug.
- **`newGame(atlas, rules)`** builds the `Game` once, in `Load`: for each shape in index order (`ShapeCount`, `Shape(i)`), its ID and name, how many patterns it has (`len(ShapePatterns(i))`), the distinct sizes of those patterns (`Pattern(p).Waypoints.Len()`), ascending, and the minimum and maximum `Pattern(p).Spell.Damage`, and the count of its patterns of each size (`PatternsByPoints`); plus `MaxPatternPoints`, `Health`, `LockedPatternPoints` and `UnlockAfterBosses` from `Rules`.
- **Reads**, all from memory:
  - `Game()`: the prebuilt `Game`; no lock, it never changes
  - `ForTeam(teamID)` → `State` (the empty state for a team with no entry). Reads return the snapshot as last applied: health is as of the last catch-up (the timer keeps it current), not recomputed on read
  - `ForUser(userID)` → `View`, or `ErrNotInTeam`
  - `CastsForUser(userID)` → `[]CastView`, newest first, or `ErrNotInTeam`; `[]` before the first cast
  - `Snapshot(userID)`: the live event for a new stream, of type `snapshot`
- **`view`** converts indexes and bits to IDs through the atlas (`WaypointIDs`, `Pattern`, `Shape`, `Waypoint`), times to UTC, builds `last_cast` from `State.LastCast()` (`null` when the log is empty), lists every stack scan with its `wrong` flag, sets `poisoned` from `State.Poisoned()`, and sets the boss fields: `bosses_defeated`, `damage_multiplier`, `health` and `knocked_out` from `State.Boss`, `unlocked_points` from `unlockedPoints` and `locked_shapes` (every shape `shapeLocked` at that size, in index order; `[]` when none), and `boss` from `Bosses.At(Defeated).Serial` with `Damage`, `PhaseStart`, `Attacks` and `next_attack_at` (`nextAttack`, `null` when knocked out), or `null` when `Defeated` ≥ `Bosses.Count()`.
- **`castView`** turns one `Cast` into a `CastView`, the shape being the pattern's `Shape`, `class` its `Spell.Class` and `damage` its precomputed `Spell.Damage` (both from `pattern`, loaded by the atlas), and `dealt`, `boss` (`null` when the cast's `Boss` is 0) and `defeated_boss` from the `Cast`. Used by `view` and `CastsForUser`.

### `write.go`: writes, each written through
- **`Select`**, **`ClearSelection`**, **`Scan`** and **`Revive`** each follow the [write-through](#the-write-through) steps, numbered in the code.
- **Catch-up first.** Right after `1. lock` and `isMember`, `Select`, `ClearSelection` and `Scan` call **`catchUp`**, which applies any boss attack that has fallen due (its own change and its own event) and returns the current state; the write validates against that. Then `Select` and `ClearSelection` refuse anyone but the leader (`isLeader`, `ErrNotLeader`, `403 not_leader`); `Select` and `Scan` refuse a knocked-out team (`ErrKnockedOut`, `409 knocked_out`); `ClearSelection` doesn't (giving up is harmless); and `Select` refuses a shape whose patterns are all locked (`shapeLocked`, `ErrShapeLocked`, `409 shape_locked`). `Revive` catches up too, so the event order is the attack, then the revive.
- **`catchUp(e, teamID)`**: `cur.Boss.at(now)`; if `Attacks` and `Health` are unchanged, returns `cur`. Otherwise it builds the next state with the new progress and `Version.Next`, **skips persist** (memory-only, AGENTS.md §6: health is derived from the stored phase and the clock, and `Load` reproduces it exactly), applies, and publishes **`boss_attacked`**, or **`knocked_out`** if health is now 0. Several due attacks become **one** change and one event.
- **`attack(teamID)`**: the attack timer's callback, run by the `Clock` in its own goroutine. `1. lock`, then nothing if the store is closed (`entry.closed`), else `catchUp` and `schedule` for the next attack. A timer that fires early finds nothing due and re-arms; one that fires late applies everything due.
- **`Revive(ctx, teamID)`**: an admin's revive. `2. validate, before 1. lock`: `Teams.ByID` (unknown → `ErrTeamNotFound`, `404 team_not_found`), `IN_GAME` (else `ErrNotInGame`, `409 not_in_game`). `1. lock`, `catchUp`, `3. build`: a new phase of the same boss, `PhaseStart` = now, `PhaseHealth` = `Health` = `Rules.Health`, `Attacks` = 0. `4. persist`: `upsertBossPhase`. `5. apply`, `6. publish` **`revived`**, and `schedule`. Allowed whether or not the team is knocked out, so it also heals; `Defeated`, `Damage` and the multiplier are kept.
- **Lock-free checks first** (§6 variation (a)): `Scan` verifies the token (`QR.Verify`, mapping its errors to `ErrInvalidQR` / `ErrExpiredQR`) and resolves the waypoint with `Atlas.WaypointIndex`, `Select` the shape with `Atlas.ShapeIndex`, all before the team and the lock.
- **`Scan`'s checks**, in order, under the lock: no selection → `ErrNoShapeSelected`; `cur.Poisoned()` → `ErrSelectionPoisoned`; the waypoint already in the stack → `ErrWaypointAlreadyScanned`. Then `Atlas.FitsShapeWithin(shape, stack ∪ {w}, unlockedPoints(cur))` decides whether the scan is **wrong** (pushed, marked `Wrong`, outcome `wrong_scan`): only patterns the team has unlocked count, so a scan that could only grow into a locked one is wrong too. Completion is `!wrong` and `Atlas.PatternFor(stack ∪ {w})` (one map lookup) being a pattern whose `Atlas.Pattern(p).Shape` is the selected shape. On completion, `Atlas.WaypointIDs` gives `ScanResult.Pattern`, `Atlas.Pattern(p).ID` is the key written to `team_pattern` and `team_cast`, `AlreadyCompleted` is `cur.Patterns.Has(p)`, the cast's damage goes to the boss (`deal(cur.Boss, Pattern(p).Spell.Damage, bosses)` sets `next.Boss` and the cast's `Dealt`, `Boss`, `DefeatedBoss`; a defeat makes the event type `boss_defeated` instead of `spell_cast`, and starts the next boss's phase, after which `Scan` calls `schedule`), and the new `Cast` is appended to the log **copy-on-write**: `append(slices.Clip(cur.Casts), cast)`. `Clip` sets the slice's capacity to its length, so `append` always allocates a new backing array and the current snapshot's slice is never written to.
- **`inGameTeam`** fetches the caller's team and requires `IN_GAME` (lock-free: a started team's status never goes back).
- **`isMember(teamID, userID)`** reads the team's current roster through `Teams.ByID` (lock-free) and reports whether the user is in it. `Select`, `ClearSelection` and `Scan` call it **again under the entry lock**, right after `1. lock`, and refuse with `ErrNotInTeam` (`404 not_in_team`) if an admin removed the caller since `inGameTeam` looked them up. A started team's roster can shrink (teams' `RemoveMember`), so the lock-free lookup alone isn't enough.
- **`isLeader(teamID, userID)`** reads the team through `Teams.ByID` (lock-free) and reports whether the user is its leader **now**. `Select` and `ClearSelection` call it under the entry lock, after `catchUp`: an admin removing the leader names a new one (teams' `RemoveMember`), and from then on only the new leader passes.
- **`RosterChanged(teamID)`** is teams' `Rules.OnRosterChange` (wired in `server.Build`): an admin removed a member from a started team. It locks the entry (`lockTeam`) and drops `entry.members`, the roster cache, so the next publish re-reads the roster from teams and no longer reaches the removed player. **Memory-only** (AGENTS.md §6): the roster is teams' data, already saved. **Publishes nothing:** the team's `State` didn't change (the removed player's scans and casts stay), and teams has already sent the rest `team` and the removed player `no_team`.
- **`PublishTeam(teamID)`** sends the team's current state to every member as `team_state`, changing nothing, and arms the team's first attack timer. teams calls it through `teams.Rules.OnStart` (wired in `server.Build`) right after a team starts, so members get the new team's empty state after its `team` event. Steps: `2. validate, before 1. lock` (`Teams.ByID`: the team exists and is `IN_GAME`, else nothing is published), `1. lock` (`lockTeam`, which creates the empty entry if the team had none, its phase starting at the team's `started_at`), `6. publish` (`publish`, which fills the entry's roster cache on this first publish of the started team), then `schedule`. Not a write: no build, persist or apply. It takes the entry lock so its event is ordered with the team's writes.
- **Declared here:** the errors (`ErrNotInGame`, `ErrUnknownWaypoint`, `ErrUnknownShape`, `ErrNoShapeSelected`, `ErrWaypointAlreadyScanned`, `ErrKnockedOut`, `ErrNotLeader` (teams' own), `ErrShapeLocked`, `ErrTeamNotFound` (teams' own, for `Revive`), `ErrSelectionPoisoned`, `ErrInvalidQR`, `ErrExpiredQR`), the `Outcome…` constants (`scan_added`, `wrong_scan`, `pattern_completed`), and `ScanResult`.

### `db.go`: SQL
Only the per-team tables. The content tables are read by the atlas (`internal/atlas/db.go`).
- **`selectTeams(ctx, teamID, stackSize)`** reads every team's stored state, or one team's (`teamID != ""`), into `teamRows`: distinct visited waypoints, completed patterns, **every** cast (`casts`, `castRow`s with `dealt`, `COALESCE(boss_serial, 0)` and `defeated_boss`, `ORDER BY team_id, id`: oldest first), the `team_boss` row if any (`boss`, a `bossRow`: `defeated`, `damage`, `multiplier`, and the nullable `phase_health`, `phase_started_at`), selection, and the stack. Used by `Load` and `reconcile`, with `stackSize` = `game.max_pattern_points` + 1. The stack query is a `JOIN LATERAL`: for each selection, the team's newest `stackSize` `team_scan` rows with `id > after_scan_id`, returned oldest first, with the `wrong` flag.
- **`insertScan`**: one `team_scan` row, with the `wrong` flag (a scan that doesn't complete anything: a correct one, or a wrong one).
- **`insertCompletingScan(…, cast, progress)`**: one `pgx.BeginFunc` transaction, five statements in order:
  1. `INSERT team_scan`: the scan itself (`wrong` is always false here)
  2. `INSERT team_pattern … ON CONFLICT DO NOTHING`: the first completion only
  3. `INSERT team_cast (team_id, pattern_id, cast_by, cast_at, already_completed, dealt, boss_serial, defeated_boss)`: every cast, repeats included; `boss_serial` is `NULL` when the cast hit no boss
  4. `upsertBoss`: `INSERT team_boss (team_id, defeated, damage, phase_health, phase_started_at) … ON CONFLICT (team_id) DO UPDATE SET` those four: the progress after the cast, and its phase (a defeat moves it to the next boss). A new row takes the default multiplier (1); an existing row keeps its multiplier, which casts never change
  5. `DELETE team_selection`: which also empties the stack
- **`upsertSelection`**: upserts `team_selection` with `after_scan_id` = the team's latest `team_scan.id` (0 if none). That's the purge: earlier scans (a wrong one included) are no longer after it.
- **`deleteSelection`**: deletes `team_selection`, and fails unless exactly one row went (memory had a selection, so Postgres must too).
- **`upsertBossPhase`**: `Revive`'s write: one `upsertBoss` on the pool (the new phase, at full health).
- **`upsertBoss(ctx, q, teamID, progress)`**: the one `team_boss` upsert, on the pool or inside a transaction: `defeated`, `damage`, `phase_health`, `phase_started_at`. The multiplier is never written here.
- **Attacks write nothing.** `catchUp` and the timer have no `db.go` call: health is derived from the stored phase.

### `store.go`: the in-memory layout
- **`Teams`** is what team state needs from the teams use case: `ForUser(userID)`, `ByID(teamID)`, and `All()` (every team: `Load` creates an entry for, and arms the timer of, every `IN_GAME` team, including those with nothing stored).
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
- **`Bosses`** is what team state needs from the bosses use case, satisfied by `*bosses.Bosses`: `Count()` (how many) and `At(i)` (the boss at index `i`, serial `i+1`). Static, so lock-free. Used by `deal` (the current boss's HP and serial), `at` and `nextAttack` (its `AttackInterval()` and `AttackDamage`), `view` (`boss.serial`) and `stateOf` (the load check).
- **`Clock`** is the store's time source, an interface with `Now()` (UTC, truncated to microseconds) and `AfterFunc(d, f) (stop func() bool)` (call `f` in its own goroutine after `d`; `stop` cancels it). **`systemClock`** is the real one: `time.Now().UTC().Truncate(time.Microsecond)` and `time.AfterFunc(d, f).Stop`.
- **`QR`** is what team state needs from the qr use case, declared here (AGENTS.md §4) and satisfied by `*qr.Verifier`: one method, `Verify(token) → (waypoint ID, error)`. It holds no state (the token is derived from the secret and the clock), so `Scan` calls it lock-free. It doesn't check the waypoint exists: `Scan` does that through `Atlas.WaypointIndex`.
- **`Rules`** are the settings from `config.toml`: `MaxPatternPoints` (`game.max_pattern_points`; the stack holds that many plus one wrong scan), `Health` (`game.health`: starting health and what a revive restores), `WriteTimeout` (`storage.write_timeout`), and `Clock` and `Epoch` (not in config.toml; nil `Clock` means `systemClock`; tests pass a fake).
- **`Load(ctx, pg, atlas, bosses, teams, qr, hub, log, rules)`** reads every team's stored state (`selectTeams` with `stackSize` = `MaxPatternPoints` + 1), checking it against the atlas and bosses, and builds the `Game` (`newGame`). Then it creates an entry for every `IN_GAME` team in `Teams.All()` that had nothing stored (`lockTeam`), so every started team is attacked, and only once the map is complete arms each entry's attack timer (`schedule`). Every state is brought up to now as it's built (`emptyState` and `stateOf` call `at`), so attacks that fell due while the backend was down are applied at load (and published to nobody: no stream is open yet; clients get them in their `snapshot`). It runs after `atlas.Load` and `bosses.Load` (it's handed both) and after `teams.Load`.
- **`Close()`** stops every pending attack timer and marks every entry `closed`, so no timer re-arms and one already running does nothing. `main.go` registers it with `srv.RegisterOnShutdown` (after `Hub.Close`); tests call it before loading another store on the same database.
- **`emptyState(teamID)`** is a `State` with an empty stack of capacity `MaxPatternPoints` + 1 and `Boss: {Defeated: 0, Damage: 0, Multiplier: 1, PhaseStart: the team's started_at (from Teams.ByID; now if it has none), PhaseHealth: Rules.Health}`, then brought up to now (`at`): a team without a `team_boss` row is at boss 1 with multiplier 1, attacked since its start.
- **`stateOf(teamID, teamRows)`** rebuilds a `State` from IDs through the atlas, failing on unknown waypoints, patterns or shapes. Every `team_cast` row becomes a `Cast` in `State.Casts`, oldest first (`cast: unknown pattern N` if the atlas lacks it). Stack rows are pushed oldest first, with their `wrong` flag. A `team_boss` row becomes `State.Boss` (`defeated`, `damage`, `multiplier`, and `phase_health` / `phase_started_at` where not `NULL`, else `emptyState`'s full health and team start), then `at(now)`; if its `defeated` is more than `Bosses.Count()` (game.bosses lowered mid-event) the load fails (`team_boss: N bosses defeated, but game.bosses is M`).
- **`schedule(e, teamID, st)`** (caller holds `e.mu`) stops the entry's pending timer (`stopAttack`) and, unless the entry is `closed` or nothing will attack (`nextAttack` not ok: knocked out, or every boss defeated), arms `Clock.AfterFunc(nextAttack − now, attack)`. Called after every apply that can move the next attack: a cast (`Scan` on completion), the timer itself, `Revive`, `reconcile`, `PublishTeam` and `Load`. A timer isn't re-armed after an ordinary scan or selection: catching up never moves the schedule forward, and a stale timer is harmless.
- **`lockTeam`** gets or creates a team's entry (with `emptyState(teamID)`) and locks it.
- **`persist`** runs a `db.go` write detached from the request, capped at **5 s** (`storage.write_timeout`).
- **`reconcile`** reloads one team's state from Postgres after a failed write (brought up to now, like `Load`), sets its `Version` to the in-memory one's `Next` (a reload never goes back), publishes it as type `resync`, and re-arms the attack timer (`schedule`). If the reload fails too, memory stays as it was.
- **`publish(e, teamID, st, typ)`**, called with `e.mu` held, sends `team_state` of type `typ` to every member. The recipients are **`e.members`**, the team's roster cached on the first publish that finds the team `IN_GAME` (its roster then only changes by an admin's removal, and `RosterChanged` drops the cache when it does). Until then, and after a drop, it reads the roster through `Teams.ByID`. So a publish reads nothing outside the entry: no index lock, no lookup.
- **`event(teamID, st, typ)`** encodes the envelope once, `live.NewEvent("team_state", typ, view)`; every member's stream gets the same bytes.
- **The type constants** (`GameStarted`, `ShapeSelected`, `SelectionCleared`, `ScanAdded`, `WrongScan`, `SpellCast`, `BossDefeated`, `BossAttacked`, `KnockedOut`, `Revived`) are declared beside `event`; `live.Snapshot` and `live.Resync` come from `platform/live`. `Scan` picks its type from its outcome: `scan_added` → `ScanAdded`, `wrong_scan` → `WrongScan`, `pattern_completed` → `SpellCast`, or `BossDefeated` when that cast defeated the boss.
- **`Rules.Epoch`** is the process's epoch (from `server.Build`). `emptyState` starts a team at `{epoch, 0}`; `Select`, `ClearSelection`, `Scan`, `Revive` and `catchUp` set `next.Version = cur.Version.Next(epoch)` in their build step. `PublishTeam` resends the current version.
- **`now`** returns `clock.Now()`: UTC, truncated to microseconds (Postgres's precision), so a restart reproduces times exactly.

## Data model: memory
```
Store
  rules    Rules                     from config.toml
  db       db                        the pool, for db.go
  atlas    Atlas                     the static content (*atlas.Atlas); read with no lock
  bosses   Bosses                    the static boss sequence (*bosses.Bosses); read with no lock
  qr       QR                        the qr use case's verifier; read with no lock
  game     Game                      built once by newGame; read with no lock
  teams    Teams                     the teams use case (*teams.Store)
  hub      *live.Hub                 where team_state events go
  log      *slog.Logger
  mu       sync.RWMutex              guards the map below; never held during I/O
  states   map[teamID]*entry         created on a team's first write (or PublishTeam)
  clock    Clock                     Rules.Clock, or systemClock: Now, and AfterFunc for attack timers

entry
  mu          sync.Mutex             writers to this team queue here
  cur         atomic.Pointer[State]  the current snapshot; readers load it lock-free
  members     []uuid.UUID            the user IDs events go to: the roster, cached
                                     by publish once the team is IN_GAME, dropped
                                     by RosterChanged on an admin's removal; nil
                                     until the next publish refills it. Guarded by mu
  stopAttack  func() bool            cancels the pending attack timer, or nil. Guarded by mu
  closed      bool                   set by Close: no timer re-arms or acts. Guarded by mu

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
    Dealt            int             damage dealt to a boss (0 if none)
    Boss             int             serial of the boss hit, 0 if none
    DefeatedBoss     bool            this cast defeated that boss
  Boss       BossProgress            a value: copied with the State
    Defeated    int                  bosses defeated; current boss = index Defeated
    Damage      int                  dealt to the current boss so far (< its HP)
    Multiplier  float64              casts' damage × this, rounded; 1 by default
    PhaseStart  time.Time            when the current boss's phase began (stored)
    PhaseHealth int                  health when it began (stored)
    Attacks     int                  attacks this phase, as of the last catch-up (derived)
    Health      int                  health as of the last catch-up (derived); 0 = knocked out
  Version    live.Version            {Epoch, Seq}; memory-only

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
- **Size:** a `State` is a fixed **1352 bytes** (`Stack` 912, `Selection` 56, the `Casts` slice header 24, `Boss` 72; measured with `unsafe.Sizeof`, linux/amd64), so a new snapshot is a plain struct copy. The cast log's backing array is on the heap, **80 bytes per cast**. 10,000 teams' states are about 13.5 MB, plus 0.8 MB for every 1 cast per team on average (20 casts each: about 16.5 MB). Each started team also holds one pending timer (a `time.Timer`, about 100 bytes).
- **Why health is derived, not stored.** Storing it would make every attack a database write: up to one per team per boss interval, all day, with nobody acting, and a write that can fail with no player to report it to. Deriving it from the phase (`PhaseStart`, `PhaseHealth`) and the clock makes an attack a memory-only change that `Load` reproduces exactly: only a defeat and a revive, which a player or admin caused, write.
- **Why the cast log is a slice and copy-on-write.** Its length isn't bounded, so it can't be a fixed array. Snapshots share the backing array until a cast: `Scan` builds the new log with `append(slices.Clip(cur.Casts), cast)`, which always copies, so a reader holding the old snapshot keeps seeing the old log. A cast costs one copy of the log (80 bytes per earlier cast), and only casts pay it: selections and scans copy the slice header only.
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
| `team_cast` | `0007`, `0009` | `id` BIGINT identity PK, `team_id` VARCHAR(6), `pattern_id` INTEGER, `cast_by` UUID, `cast_at` TIMESTAMPTZ, `already_completed` BOOLEAN, all `NOT NULL`; from `0009`: `dealt` INTEGER `NOT NULL DEFAULT 0 CHECK (dealt >= 0)`, `boss_serial` INTEGER NULL → `boss (serial)`, `defeated_boss` BOOLEAN `NOT NULL DEFAULT false` | One row per **cast**, repeats included (unlike `team_pattern`): the cast log. `pattern_id` → `pattern` (a cast is of a real pattern); `cast_by` → `userprofile` (a real user); `already_completed` records whether `team_pattern` already had the pattern. `id` orders casts: `Load` reads them `ORDER BY team_id, id`. Index `team_cast_team_id_idx` on `(team_id, id DESC)` serves a one-team read (`reconcile`). No unique constraint: any number of casts per team and pattern. `boss_serial` → `boss` (a cast hits a real boss); `NULL` for a cast after every boss was defeated, or from before `0009` (whose `dealt` defaults to 0) |
| `team_boss` | `0009`, `0010` | `team_id` VARCHAR(6) PK, `defeated` INTEGER `DEFAULT 0 CHECK (>= 0)`, `damage` INTEGER `DEFAULT 0 CHECK (>= 0)`, `multiplier` DOUBLE PRECISION `DEFAULT 1 CHECK (> 0)`, all `NOT NULL`; from `0010`: `phase_health` INTEGER NULL `CHECK (>= 0)`, `phase_started_at` TIMESTAMPTZ NULL | One row per team with boss progress (PK `team_id`); no row means boss 1, 0 damage, multiplier 1, a phase from the team's start at full health. `phase_health` `NULL` means full health (`game.health`), `phase_started_at` `NULL` the team's `started_at`. Written by `insertCompletingScan`'s transaction (every cast) and by `Revive` (`upsertBossPhase`), both through `upsertBoss`. Health and attacks aren't columns: they're derived. The CHECKs back "progress never negative", "the multiplier is positive" and "health never negative". That `defeated` ≤ `game.bosses` and `damage` < the boss's HP are checked in memory (Load, `deal`), since they depend on config and another table |

Every per-team table has `team_id` → `team` with `ON DELETE CASCADE`. `team_cast` is written only inside `insertCompletingScan`'s transaction, so a cast is logged, and its damage counted, exactly when its scan and its `team_pattern` row are; `team_boss` is written there too, and by a revive.

**Migration `0010`'s SQL comment states the attack formula without the knockout cap, and with the boss's `attack_interval_s` / `attack_damage` columns**, which migration `0011` dropped: the interval and damage now come from `config.toml` (`[game.boss_attack]`), through `bosses.Rules`. The code and this README are right: attacks stop at the knockout attack, `ceil(phase_health / attack_damage)` (see "Boss attacks and health"). An applied migration can't be edited (AGENTS.md §8: its checksum is recorded); a later migration can correct the comment in the database with `COMMENT ON` if it's ever worth one.

**The stack isn't a table.** It's derived: the `team_scan` rows with `id > after_scan_id` (at most **7**, `game.max_pattern_points` + 1, which is the query's `LIMIT`). Selecting moves `after_scan_id` to the latest scan (purge); completing or clearing deletes the selection (no selection, no stack). Scans never leave an attempt any other way, so the rows after `after_scan_id` are exactly the stack.

## Memory ↔ Postgres
| Postgres | Rebuilt in memory (by `Load`, or `reconcile` for one team) |
|---|---|
| `waypoint`, `shape`, `pattern`, `pattern_waypoint` | Not here: the atlas, rebuilt by `atlas.Load` before this `Load` ([`internal/atlas/README.md`](../atlas/README.md)) |
| `DISTINCT waypoint_id FROM team_scan` | `State.Waypoints` |
| `team_pattern` | `State.Patterns`, and `State.Shapes` from each pattern's shape |
| `team_selection` | `State.Selection` |
| every `team_cast` row, `ORDER BY team_id, id` | `State.Casts`, oldest first (with `dealt`, `boss_serial` as 0 for `NULL`, `defeated_boss`); `last_cast` is its last element |
| `team_boss` | `State.Boss` (`Defeated`, `Damage`, `Multiplier`, `PhaseHealth`, `PhaseStart`); no row → boss 1, 0, multiplier 1, full health from the team's `started_at` |
| `team.started_at` (through `Teams.ByID`) | `PhaseStart` when `phase_started_at` is `NULL` or there's no row |
| (none: derived from the phase, `game.boss_attack.interval`, the boss's `game.boss_attack.damage` and the clock) | `BossProgress.Attacks`, `Health`: recomputed by `at(now)` at `Load`, so attacks that fell due while the backend was down are applied at once |
| (none: memory-only) | `entry.stopAttack` (the pending timer): re-armed by `Load` for every started team |
| `boss` | Not here: the bosses use case, loaded by `bosses.Load` before this `Load` ([`internal/bosses/README.md`](../bosses/README.md)) |
| newest **7** (`game.max_pattern_points` + 1) `team_scan` with `id > after_scan_id`, by `id` | `State.Stack` (pushed oldest first, with the `wrong` flag, so `poisoned` comes back too) |
| (none: built from the atlas and `config.toml`) | `Store.game`, rebuilt by `newGame` at every `Load` |
| (none: memory-only, from teams) | `entry.members`, the roster cache: refilled from `Teams.ByID` on each team's first publish after a restart. teams reloads rosters from `teammember` (an admin's removal deleted the row), and `RosterChanged` drops the cache on every removal, so the cache always matches |
| (none: memory-only) | `State.Version`: `Load` starts every team at seq 0 in the new process's epoch, which clients treat as newer than anything from the old process, so nothing needs storing |

**Nothing else is held only in memory.** The attack timers are rebuilt by `Load`, and health is a function of what's stored and the clock, so a restart reproduces memory exactly at the same moment, versions aside: `TestRestartRestoresEverything`, `TestCastLog`, `TestCastsDamageBosses` and `TestAttacksAcrossRestart` compare the JSON before and after.

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

## Bosses
Every team fights the same **6** (`game.bosses`) bosses in serial order. The bosses themselves are static ([`internal/bosses/README.md`](../bosses/README.md)); a team's place among them is `State.Boss`, changed only by casts.

**What a cast does** (`deal`, called by `Scan` on `pattern_completed`):
1. If every boss is defeated (`Defeated` = `Count()`, or no bosses are loaded): nothing. The cast still counts (pattern, shape, cast log) with `dealt 0`, `boss null`.
2. Otherwise `dealt = math.Round(pattern damage × Multiplier)` (halves away from zero: 205 × 1.5 = 307.5 → 308), added to `Damage`.
3. If `Damage` reaches the current boss's HP (≥): `Defeated` + 1, `Damage` = 0. **The excess is lost**, never carried to the next boss, so one cast defeats at most one boss. The event type is `boss_defeated`. The next boss's **phase starts at the cast**, with the health the team has then: its attack clock starts from zero.
4. The cast records `dealt`, `boss` (the serial hit) and `defeated_boss`; the progress is upserted into `team_boss` in the cast's transaction.

**Repeats count.** A cast of an already-completed pattern (`already_completed: true`) deals damage like any other.

**Defeating boss 5 unlocks the big patterns** (`game.unlock_after_bosses`, **5**): `unlocked_points` goes from 4 to 6 and `locked_shapes` empties in the same `boss_defeated` event.

**The multiplier** is stored per team (`team_boss.multiplier`) and applied to every cast, but **nothing sets it yet**: it's 1 for every team unless changed in Postgres with the backend stopped. A future power-up would change it as a teamstate write.

**What the client shows** (`/play`): the current boss from `GET /api/game/bosses` by `boss.serial` (image, name, description), an HP bar of `hp − boss.damage` of `hp`, the multiplier when it isn't 1, "Every boss defeated!" when `boss` is `null`; a `boss_defeated` event shows "<name> defeated!" on every phone; the cast log shows each cast's `dealt` and the boss it hit.

## Boss attacks and health
Every team has health, **10** (`game.health`) at the start. Its current boss attacks it every **10 min** of the phase (`game.boss_attack.interval`, the same for every boss), for its damage (`[game.boss_attack.damage]` by serial: boss 1 does 1 … boss 6, Gideon, does 6, for now). Both come from `config.toml` through `bosses.Rules`, filled into each `bosses.Boss` (`AttackInterval()`, `AttackDamage`). At full health a team stalled on boss 1 lasts 100 min (10 attacks); on the twins or Gideon, 20 min (2 attacks); health carries over between bosses. This is the game's one change caused by time passing, so the server applies and publishes it with nobody acting (AGENTS.md §9: the server owns time).

**Health is derived, never stored per attack.** The phase is what's stored (`team_boss.phase_started_at`, `phase_health`), and at any time `now`:

```
attacks = min(floor((now − PhaseStart) / interval),     the k-th attack is at PhaseStart + k × interval
              ceil(PhaseHealth / attack_damage))         ...and the boss stops at the knockout attack
health  = max(0, PhaseHealth − attacks × attack_damage)
```

The cap matters only once the team is knocked out: without it, a knocked-out team's `boss.attacks` stayed at the knockout attack in memory but was recomputed from every elapsed interval by `Load`, so a restart changed it. `TestGameLoopSimulation` caught that (a team on boss 7 knocked out after 2 attacks came back with 7). With `attack_damage` 0 there's no knockout and no cap.

So an attack writes nothing: it's a memory-only change that `Load` reproduces exactly (the kind AGENTS.md §6 allows). Only a phase boundary writes:

| What | New phase | Written by |
|---|---|---|
| Team start | boss 1, from `team.started_at`, full health | nothing: `NULL` phase columns (or no row) mean exactly this |
| A cast defeats the boss | the next boss, from the cast's time, with the health then | `insertCompletingScan` (`upsertBoss`) |
| An admin's revive | the same boss, from now, full health | `Revive` (`upsertBossPhase`) |
| Every boss defeated | none: nothing attacks | the defeating cast |

**How an attack is applied.**
1. **The timer.** `schedule` arms `Clock.AfterFunc` for `nextAttack` = `PhaseStart + (attacks + 1) × interval`. When it fires, `attack` locks the entry, calls `catchUp` and re-arms.
2. **`catchUp`** computes `at(now)`; if anything fell due it bumps the version, applies, and publishes `boss_attacked`, or `knocked_out` if health hit 0. No persist.
3. **Every write catches up first.** `Select`, `ClearSelection`, `Scan` and `Revive` call `catchUp` right after taking the lock. So a timer that's late (a busy process, or the lock held by a write) never loses an attack: the write applies it first, with its own event, and then validates against the real health. A cast that defeats a boss is dealt after the attacks due before it.
4. **Several due attacks are one change.** A late timer, or a restart after downtime, applies every attack due as one `boss_attacked` (or `knocked_out`) event, with `boss.attacks` up by that many.

**Knocked out** (health 0): `Select` and `Scan` are refused with `409 knocked_out` (nothing written, no event); `ClearSelection` still works; `nextAttack` is not ok, so no timer is armed and nothing attacks. The progress, patterns, cast log and selection are kept.

**Revive** (`POST /api/admin/teams/{team_id}/revive`, admin only): health back to 10 and a new phase of the same boss from now (its earlier attacks no longer count); `revived` to every member; the timer re-armed. Allowed at any time, so it also heals a team that isn't knocked out.

**Config changes apply retroactively.** Health is recomputed from the phase start, so `game.boss_attack.interval` or a boss's damage changed in `config.toml` between restarts changes every current phase against it (see [Not decided yet](#not-decided-yet)).

**What the client shows** (`/play`): a health bar of `health` of `Game.health`; the current boss's interval and damage, and a countdown to `boss.next_attack_at` on the server's clock (`hello`, `ServerClock`); when `knocked_out`, a red banner and the type buttons and scan form disabled; on `boss_attacked`, `knocked_out` and `revived`, a message on every phone. The client only displays: the server applies attacks.

## Walkthroughs
Team `482913`, members A–F, **F the leader** (only F selects and clears), 2025 seed (square = `1001 1002 1007 1008` = indexes 0, 1, 6, 7, bitmask 195; `square` = shape 2), config.toml as shipped. Times are `mm:ss` after 12:00. Stacks are written bottom → top as `waypoint/member@time`; a wrong scan is marked ✗.

### 1. A square, with every rejection
| Time | Request | Check | Stack after | Postgres after | Response |
|---|---|---|---|---|---|
| 00:00 | A: select `square` | **A isn't the leader** | empty | unchanged | `403 not_leader` |
| 00:00 | F: select `square` | team IN_GAME, shape known, F leads, `square` (4 points) not locked | empty | `team_selection(square, F, after_scan_id = latest)` | `200`, `selection.shape = "square"` |
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
| 00:12 | A completes square `1001 1002 1007 1008` (walkthrough 1) | `{"pattern":["1001","1002","1007","1008"],"shape":"square","class":"class_4","cast_by":A,"cast_at":"…12:00:12Z","already_completed":false,"damage":205,"dealt":205,"boss":1,"defeated_boss":true}` (205 ≥ boss 1's 25 HP) | `[that cast]` | `[[1001,1002,1007,1008]]` | 1 | 1 |
| 01:00 | F selects `triangle` | **unchanged** (a selection doesn't clear it) | unchanged | unchanged | 1 | 1 |
| 02:00 | F selects `square` again | unchanged | unchanged | unchanged | 1 | 1 |
| 02:01–02:04 | A, B, C, D scan the same square; D's scan completes it | `{…,"cast_by":D,"cast_at":"…12:02:04Z","already_completed":true,"dealt":205,"boss":2,"defeated_boss":true}` (205 ≥ boss 2's 100 HP) | `[02:04 cast (already_completed: true), 00:12 cast]` | unchanged (counted once) | 2 | 1 (`ON CONFLICT DO NOTHING`) |
| (restart) | | the 02:04 cast | the same two, same order | unchanged | 2 | 1 |

`ScanResult.already_completed` and `last_cast.already_completed` agree for the scan that cast.

### 7. Casts through the first bosses (seed HP 25, 100, 250; square damage 205; multiplier 1)
| Cast | `boss` before | `dealt` | `boss` after | `bosses_defeated` | `team_boss` after (defeated, damage, multiplier) | Event type |
|---|---|---|---|---|---|---|
| 00:12 | `{serial 1, damage 0}` | 205 | `{serial 2, damage 0}` (205 − 25 = 180 lost) | 1 | `(1, 0, 1)` (row inserted) | `boss_defeated` |
| 02:04 | `{serial 2, damage 0}` | 205 | `{serial 3, damage 0}` (105 lost) | 2 | `(2, 0, 1)` | `boss_defeated` |
| 04:00 | `{serial 3, damage 0}` | 205 | `{serial 3, damage 205}` | 2 | `(2, 205, 1)` | `spell_cast` |

With `team_boss.multiplier = 1.5` instead, each cast deals 308 (307.5 rounded away from zero): the same two defeats, then boss 3 at 308.

After the sixth boss (Gideon) falls, `boss` is `null`, `bosses_defeated` 6, and further casts have `dealt 0`, `boss null`, type `spell_cast`. The fifth defeat (the Katayanagi twins) is also the unlock: from that `boss_defeated` event on, `unlocked_points` is 6 and `locked_shapes` is `[]`.

### 8. Boss attacks, a knockout and a revive (`game.boss_attack`: every 10 min, boss 1 for 1, boss 2 for 2; `game.health` 10)
The team started at 12:00:00. Times are absolute.

| Time | What happens | `health` | `boss` after | `team_boss` after | Event |
|---|---|---|---|---|---|
| 12:00:00 | start (`PublishTeam`); timer armed for 12:10:00 | 10 | `{serial 1, attacks 0, phase_started_at 12:00:00, next_attack_at 12:10:00}` | no row | `game_started` |
| 12:09:59.999999 | (nothing due) | 10 | unchanged | no row | none |
| 12:10:00 | the timer: `catchUp` finds 1 attack | 9 | `{…, attacks 1, next_attack_at 12:20:00}` | no row (attacks aren't written) | `boss_attacked` |
| 12:20:00 | the timer | 8 | `{…, attacks 2, next_attack_at 12:30:00}` | no row | `boss_attacked` |
| 12:22:30 | D's scan casts and defeats boss 1 | 8 | `{serial 2, damage 0, attacks 0, phase_started_at 12:22:30, next_attack_at 12:32:30}`; timer re-armed | `(1, 0, 1, phase_health 8, phase_started_at 12:22:30)` | `boss_defeated` |
| 12:32:30 … 12:52:30 | the timer, every 10 min, 2 a hit: 3 attacks | 6, 4, 2 | `attacks` 1 … 3 | unchanged | `boss_attacked` × 3 |
| 13:02:30 | the 4th attack | **0** | `{…, attacks 4, next_attack_at null}`; no timer | unchanged | **`knocked_out`** |
| 13:03:00 | F selects `square` | 0 | unchanged | unchanged | `409 knocked_out`, none |
| 13:03:05 | F clears the selection (allowed while knocked out) | 0 | unchanged | unchanged | `200`, a no-op here (the cast had cleared the selection) |
| 13:10:00 | admin revives | **10** | `{serial 2, attacks 0, phase_started_at 13:10:00, next_attack_at 13:20:00}`; timer re-armed | `(1, 0, 1, phase_health 10, phase_started_at 13:10:00)` | `revived` |

**A restart at 12:35:00** (between the 12:32:30 attack and the next) reloads `phase_health 8, phase_started_at 12:22:30` from `team_boss`, computes `attacks = floor(750 s / 600 s) = 1`, `health = 8 − 1 × 2 = 6`, the same as before, and arms the timer for 12:42:30. **A backend down from 12:35:00 to 12:55:00** reloads with `attacks = floor(1950 / 600) = 3`, `health 2`: the two attacks it missed are applied at load, and clients see them in their `snapshot`.

**A late timer.** If the 12:10:00 timer hasn't run when a scan arrives at 12:10:00.2, the scan's `catchUp` applies the attack first (`boss_attacked`), then the scan (`scan_added`): two events, in that order. The timer then finds nothing due and re-arms for 12:20:00.

## The write-through
Every write in `write.go` (`Select`, `ClearSelection`, `Scan`, `Revive`) runs these steps, marked in the code (`PublishTeam` is not a write, and `catchUp` is a memory-only change; see the table):
1. **lock** the team's entry (`lockTeam`)
2. **validate**:
   - first the atlas lookup (shape or waypoint ID) and the team, **before** the lock (an `IN_GAME` team's status never goes back). This is AGENTS.md §6 variation (a); the code labels it `2. validate, before 1. lock`
   - then, under the lock, membership again (`isMember`: an admin may have removed the caller since the lookup), then `catchUp` (any due boss attack, its own change), then the `State` (`2. validate (state)`: knocked out first for `Select` and `Scan`)
3. **build** the next `State` (a copy; the cast log copy-on-write)
4. **persist**: one `db.go` call (`insertScan`, `insertCompletingScan`, `upsertSelection`, `deleteSelection` or `upsertBossPhase`), detached from the request and capped at **5 s** (`storage.write_timeout`)
5. **apply**: `entry.cur.Store(&next)`
6. **publish** `team_state` to every member, still holding the lock so a team's events are in order; a cast or revive then re-arms the attack timer (`schedule`)

| Write | `db.go` call (step 4) |
|---|---|
| `Select` | `upsertSelection` |
| `ClearSelection` | `deleteSelection` |
| `Scan`, no completion (wrong included) | `insertScan` |
| `Scan` that casts | `insertCompletingScan` (`team_scan`, `team_pattern`, `team_cast`, upsert `team_boss`, delete `team_selection`, one transaction) |
| `Revive` (admin) | `upsertBossPhase` |
| `catchUp` (the attack timer, and the start of every write) | none: **memory-only** (AGENTS.md §6). Health is derived from the stored phase, so `Load` reproduces the change. Steps 3, 5 and 6 |
| `PublishTeam` (start hook) | none: not a write. Only steps 2, 1 and 6; the state is published as it is, then the timer is armed |

**On a failed persist:** `reconcile` reloads the team from Postgres (Postgres wins), publishes the result, and returns the error, which becomes a `500`. Nothing from the failed write is applied.

**Rejections and no-ops return before step 4:** every `409` (`selection_poisoned` and `knocked_out` included) and clearing when there's nothing to clear. A rejection after a `catchUp` that applied an attack still leaves that attack applied and published: it's its own change. Nothing is written and nothing is published. A **wrong scan is not a rejection**: it's written and published. Reselecting is **not** a no-op: it always purges, writes and publishes.

## API
All routes need `Authorization: Bearer <token>` (`401` without). Bodies over **1 MiB** (`server.max_body_bytes`) get `400 invalid_body`.

| Method | Path | Body | Response payload |
|---|---|---|---|
| GET | `/api/game/shapes` | none | `Game` |
| GET | `/api/teams/me/state` | none | `View` |
| GET | `/api/teams/me/casts` | none | `[CastView]`, newest first |
| PUT | `/api/teams/me/state/selection` | `{"shape": "square"}` | `View` |
| DELETE | `/api/teams/me/state/selection` | none | `View` |
| POST | `/api/teams/me/state/scans` | `{"qr": "1007:1760000000.nR5…"}` | `ScanResult` |
| POST | `/api/admin/teams/{team_id}/revive` | none; **admin only** (`403 not_admin`) | `View` |

**`View`** (also the data of the `team_state` live event), here for a poisoned attempt:
```json
{
  "team_id": "482913",
  "waypoints": ["1001", "1002", "1003"],
  "patterns": [["1003", "1004", "1005", "1006"]],
  "shapes": ["square"],
  "selection": {"shape": "square", "selected_by": "6f1c…", "selected_at": "2026-10-17T12:00:00Z"},
  "last_cast": {"pattern": ["1003", "1004", "1005", "1006"], "shape": "square", "class": "class_4", "cast_by": "9a2b…", "cast_at": "2026-10-17T11:58:40Z", "already_completed": false, "damage": 198, "dealt": 198, "boss": 3, "defeated_boss": false},
  "stack": [
    {"waypoint_id": "1001", "user_id": "9a2b…", "scanned_at": "2026-10-17T12:00:01Z", "wrong": false},
    {"waypoint_id": "1002", "user_id": "c3d4…", "scanned_at": "2026-10-17T12:00:02Z", "wrong": false},
    {"waypoint_id": "1003", "user_id": "e5f6…", "scanned_at": "2026-10-17T12:00:03Z", "wrong": true}
  ],
  "poisoned": true,
  "bosses_defeated": 2,
  "boss": {"serial": 3, "damage": 198, "phase_started_at": "2026-10-17T11:45:00Z", "attacks": 1, "next_attack_at": "2026-10-17T12:05:00Z"},
  "damage_multiplier": 1,
  "health": 9,
  "knocked_out": false,
  "unlocked_points": 4,
  "locked_shapes": [],
  "cast_count": 1,
  "version": {"epoch": 1760702400000, "seq": 4}
}
```
- `selection` is `null` when nothing is selected.
- `last_cast` is the team's newest cast, or `null` if it has none (or cast only before migration `0007`). `pattern` is its sorted waypoint IDs, `shape` its shape ID, `class` its spell class ID (`class_4` for a 4-point pattern: the class of its waypoint count), `cast_by` the member whose scan completed it, `cast_at` that scan's time, `already_completed` true if the team had cast this pattern before (progress didn't grow), and `damage` is what the cast spell deals (`pattern.damage`). It stays until the next cast: selecting or clearing doesn't remove it.
- `stack` is the attempt's scans, bottom (oldest) first; `[]` when empty. `wrong` is true for the scan that didn't fit (at most one, always the top).
- `poisoned` is true while the stack holds a wrong scan: scans are refused with `selection_poisoned` until a reselect.
- `last_cast.dealt` is what the cast dealt to a boss (`damage` × the multiplier then, rounded; 0 if none), `boss` the serial it hit (`null` if none), `defeated_boss` whether it finished it.
- `bosses_defeated` is how many bosses the team has defeated. `boss` is the one it's fighting: its `serial` (name, HP and asset are in `GET /api/game/bosses`) and the `damage` dealt to it so far, always below its HP; `null` once every boss is defeated (or none are loaded). `damage_multiplier` is what each cast's damage is multiplied by, 1 by default.
- `boss.phase_started_at` is when the team began fighting it (or was last revived); `boss.attacks` how many times it has attacked since; `boss.next_attack_at` when it attacks next, or `null` if the team is knocked out. Clients count down to it on the server's clock (`hello`).
- `health` is the team's health (**10**, `game.health`, at the start, less every attack); `knocked_out` is true at 0.
- `unlocked_points` is the most waypoints a pattern the team can cast may have: **4** (`game.locked_pattern_points` − 1) until it has defeated **5** (`game.unlock_after_bosses`) bosses, then **6** (`game.max_pattern_points`). `locked_shapes` are the shapes it can't select yet (every pattern bigger), in index order; `[]` with the 2025 seed, whose patterns are all 3- or 4-point.

**Cast log** (`GET /api/teams/me/casts`), after walkthrough 6:
```json
[
  {"pattern": ["1001", "1002", "1007", "1008"], "shape": "square", "class": "class_4", "cast_by": "<D>", "cast_at": "2026-10-17T12:02:04Z", "already_completed": true, "damage": 205, "dealt": 205, "boss": 2, "defeated_boss": true},
  {"pattern": ["1001", "1002", "1007", "1008"], "shape": "square", "class": "class_4", "cast_by": "<A>", "cast_at": "2026-10-17T12:00:12Z", "already_completed": false, "damage": 205, "dealt": 205, "boss": 1, "defeated_boss": true}
]
```
- Newest first; `[]` before the first cast, or for a team in the lobby.
- Read from memory, like every other read. Not pushed on the stream: `team_state.last_cast` carries each new cast, so a client loads the log once and prepends each new `last_cast` it sees (by `cast_at`), which is what `/play` does.

**`Game`** (`GET /api/game/shapes`), with the 2025 seed and config.toml as shipped:
```json
{
  "shapes": [
    {"id": "collinear", "name": "Collinear", "patterns": 9,   "points": [3], "damage": {"min": 66, "max": 83}, "patterns_by_points": {"3": 9}, "classes": ["class_3"]},
    {"id": "lightning", "name": "Lightning", "patterns": 77,  "points": [4], "damage": {"min": 196, "max": 282}, "patterns_by_points": {"4": 77}, "classes": ["class_4"]},
    {"id": "square",    "name": "Square",    "patterns": 18,  "points": [4], "damage": {"min": 199, "max": 214}, "patterns_by_points": {"4": 18}, "classes": ["class_4"]},
    {"id": "tree",      "name": "Tree",      "patterns": 99,  "points": [4], "damage": {"min": 196, "max": 288}, "patterns_by_points": {"4": 99}, "classes": ["class_4"]},
    {"id": "triangle",  "name": "Triangle",  "patterns": 156, "points": [3], "damage": {"min": 64, "max": 85}, "patterns_by_points": {"3": 156}, "classes": ["class_3"]}
  ],
  "health": 10,
  "locked_pattern_points": 5,
  "unlock_after_bosses": 5,
  "max_pattern_points": 6,
  "spell_classes": [{"id": "class_1", "name": "Class 1", "points": 1}, {"id": "class_2", "name": "Class 2", "points": 2}, …, {"id": "class_15", "name": "Class 15", "points": 15}]
}
```
- `shapes`: every selectable shape (the client's "types"), in index order (by ID). `id` is what `PUT …/selection` takes; `name` is for display; `patterns` is how many valid patterns the shape has; `points` is the distinct sizes of those patterns, ascending (a shape mixing 3- and 4-point patterns shows `[3, 4]`); `damage` is the minimum and maximum damage across the shape's patterns; `patterns_by_points` counts its patterns by size (JSON keys are the sizes): a team can cast those of at most its `team_state.unlocked_points`.
- `health`: **10** (`game.health`), every team's starting health and what a revive restores.
- `locked_pattern_points`: **5** (`game.locked_pattern_points`): patterns of at least this many waypoints are locked until a team has defeated `unlock_after_bosses` (**5**, `game.unlock_after_bosses`) bosses. Whether a given team has is in its `team_state` (`unlocked_points`, `locked_shapes`), not here: this route is the same for everyone.
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
- `pattern_completed`: `pattern` is set; `already_completed` is true if the team had completed this pattern before. The stack and selection are purged, `state.last_cast` is the new cast (with what it dealt to which boss), and `state.boss` / `state.bosses_defeated` show the progress after it.

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
| `ErrKnockedOut` | 409 | `knocked_out` |
| `ErrNotLeader` (`Select`, `ClearSelection`) | 403 | `not_leader` |
| `ErrShapeLocked` (`Select`) | 409 | `shape_locked` |
| `ErrTeamNotFound` (`Revive`) | 404 | `team_not_found` |
| `ErrInvalidQR` | 400 | `invalid_qr` |
| `ErrExpiredQR` | 409 | `qr_expired` |
| a body that doesn't decode | 400 | `invalid_body` |

The revive route also answers `401` (`missing_token`, `invalid_token`, `user_not_found`) and `403 not_admin` from the admin middleware.

**Live events:** `team_state`, its data the envelope `{"type", "message"}` with the `View` as `message`, sent:

| When | `type` | To |
|---|---|---|
| a member connects to `GET /api/events` (right after `team`) | `snapshot` | that stream |
| the leader starts (`PublishTeam`, through teams' `Rules.OnStart`, right after `team: started`): the empty state | `game_started` | members |
| `Select` (a spell attempt starts; the stack is purged) | `shape_selected` | members |
| `ClearSelection` | `selection_cleared` | members |
| `Scan`, outcome `scan_added` | `scan_added` | members |
| `Scan`, outcome `wrong_scan` | `wrong_scan` | members |
| `Scan`, outcome `pattern_completed` (a cast: new `last_cast`, `cast_count` + 1, damage to the boss) | `spell_cast` | members |
| `Scan`, outcome `pattern_completed`, and the cast defeated the boss (`bosses_defeated` + 1, `boss` the next or `null`) | `boss_defeated` | members |
| a boss attack falls due (`catchUp`, from the timer or the start of a write), with **nobody acting** | `boss_attacked` | members |
| ... and it brought health to 0 | `knocked_out` | members |
| an admin's `Revive` | `revived` | members |
| `reconcile` | `resync` | members |

Never after a rejection or no-op. Every change bumps `version.seq` by one; `snapshot` and `game_started` repeat the current version. The responses (`GET` state, both selection routes, `ScanResult.state`) carry the same `version`, so a client keeps whichever copy is newest.

The only events published with nobody acting are boss attacks (`boss_attacked`, `knocked_out`), from the attack timer. Every trigger, the ordering guarantees and the wire format: [`internal/server/README.md`, "The event stream"](../server/README.md#the-event-stream).

## Rules and cases

### Loading at startup
The backend refuses to start if any of these hold:

| Case | Error |
|---|---|
| `config.toml` is invalid (see `internal/platform/config`), e.g. a missing key, `max_pattern_points` outside 1–15, `locked_pattern_points` outside 1–`max_pattern_points` + 1, or `unlock_after_bosses` outside 0–`game.bosses` | from `config.Load`, before anything loads |
| The content is invalid (bad waypoint IDs, too many items, unknown, empty, oversized, duplicate or nested patterns, missing damage) | from `atlas.Load`, before this `Load` runs: every case, in order, is in [`internal/atlas/README.md`](../atlas/README.md), "Loading at startup" |
| Reading the per-team tables fails | `load team state: …` |
| Stored visited waypoints name a waypoint the atlas doesn't have | `team 482913 state: unknown waypoint "1012"` |
| A `team_pattern` key the atlas doesn't have | `team 482913 state: unknown pattern 400` |
| A selection's shape the atlas doesn't have | `team 482913 state: selection: unknown shape "hexagon"` |
| A `team_cast` row's pattern the atlas doesn't have | `team 482913 state: cast: unknown pattern 400` |
| A stack scan's waypoint the atlas doesn't have | `team 482913 state: stack scan: unknown waypoint "1012"` |
| The boss content is invalid (wrong count for `game.bosses`, gaps, no name or asset, HP ≤ 0, attack interval ≤ 0, attack damage < 0) | from `bosses.Load`, before this `Load`: [`internal/bosses/README.md`](../bosses/README.md), "Loading at startup" (an empty `boss` table is allowed) |
| A `team_boss` row with more bosses defeated than `game.bosses` | `team 482913 state: team_boss: 7 bosses defeated, but game.bosses is 6` |

### Select: `PUT /api/teams/me/state/selection`
Checked in this order:

| Case | Result |
|---|---|
| Body not `{"shape": string}`, unknown fields, empty, or over 1 MiB | `400 invalid_body` |
| Shape unknown | `404 unknown_shape` |
| Caller not in a team | `404 not_in_team` |
| Team not `IN_GAME` | `409 not_in_game` |
| (any boss attack that fell due is applied first: `boss_attacked` / `knocked_out`) | |
| Caller isn't the team's leader (read now, under the lock) | `403 not_leader`: nothing written, no event |
| Team knocked out | `409 knocked_out`: nothing written, no event |
| Every pattern of the shape has more waypoints than the team's `unlocked_points` (before `game.unlock_after_bosses` defeats: 5+ points) | `409 shape_locked`: nothing written, no event |
| A shape mixing locked and unlocked sizes | Allowed: only its unlocked patterns can be cast (see Scan) |
| Same shape already selected | `200`: **stack purged** (a wrong scan included: `poisoned: false`), selection re-stamped with the caller and now; written; event |
| A different shape, or nothing selected | `200`: selection set; stack purged; written; event |
| Shape already completed before | Allowed; completing another of its patterns adds the pattern |

### Clear: `DELETE /api/teams/me/state/selection`
| Case | Result |
|---|---|
| Caller not in a team | `404 not_in_team` |
| Team not `IN_GAME` | `409 not_in_game` |
| (any boss attack that fell due is applied first) | |
| Caller isn't the team's leader | `403 not_leader`: nothing written, no event |
| Team knocked out | Allowed: clearing only gives up |
| Nothing selected | `200`, no-op: nothing written, no event |
| Otherwise | `200`: selection and stack removed (a wrong scan included); visited waypoints, completions and the cast log kept; event |

### Scan: `POST /api/teams/me/state/scans`
Checked in this order. Every rejection changes nothing, isn't logged and sends no event.

| Case | Result |
|---|---|
| Body not `{"qr": string}`, empty, or over 1 MiB | `400 invalid_body` |
| Token malformed or its signature doesn't check out | `400 invalid_qr` |
| Token signed but outside its validity window (`qr.rotate` + `qr.tolerance`) | `409 qr_expired` |
| Waypoint unknown (the token names a waypoint the atlas doesn't have) | `404 unknown_waypoint` |
| Caller not in a team | `404 not_in_team` |
| Team not `IN_GAME` | `409 not_in_game` |
| (any boss attack that fell due is applied first: `boss_attacked` / `knocked_out`) | |
| Team knocked out | `409 knocked_out` |
| No shape selected (including right after a completion) | `409 no_shape_selected` |
| The stack holds a wrong scan (poisoned), whatever the waypoint, whoever scans | `409 selection_poisoned` |
| Waypoint already in the stack, by anyone (a retry included) | `409 waypoint_already_scanned` |
| Caller already has scans in the stack | Allowed: any member may scan any number of waypoints |
| Any time since the selection or the last scan, however long | Allowed: there's no time limit |
| The waypoint, with the stack, isn't part of any pattern of the selected shape **of at most `unlocked_points` waypoints** (a set only a locked pattern contains is wrong) | `200 wrong_scan`: pushed with `wrong: true`, `poisoned: true`; logged with `wrong = true`; event |
| Stack plus this scan is exactly a pattern of the **selected** shape | `200 pattern_completed`: pattern and shape marked; **stack and selection purged**; cast appended to the log and `last_cast` (`already_completed: false`); `team_cast` +1; the damage × multiplier dealt to the current boss, `team_boss` upserted; event `spell_cast` |
| ... and the damage brings the boss to its HP | The boss is defeated: `bosses_defeated` + 1, `boss` the next one at 0 damage (or `null` after the last); the excess is lost; `defeated_boss: true`; the next boss's phase starts now with the current health (`team_boss.phase_*`), timer re-armed; event `boss_defeated` |
| ... and every boss is already defeated (or none are loaded) | Cast as usual with `dealt: 0`, `boss: null`; progress unchanged; event `spell_cast` |
| ... that pattern was already completed | `pattern_completed` with `already_completed: true`; progress unchanged; cast appended (`already_completed: true`); `team_cast` +1, `team_pattern` unchanged |
| Stack plus this scan is a pattern of **another** shape | Not completed → `scan_added` |
| Otherwise | `scan_added` |
| 1-point pattern | Completes on its only scan |
| 6-point pattern, after the unlock | Completes on the sixth scan (the stack never needs its seventh slot for correct scans) |
| 5- or 6-point pattern before the unlock | Can't complete: the scan that would leave the 4-point patterns is wrong |
| A non-leader scanning | Allowed: every member scans |
| Any accepted scan (wrong included) | Logged in `team_scan` (with `wrong`); waypoint added to visited `waypoints` |
| Postgres write fails | `500`; state reloaded from Postgres; nothing applied |

### Start: `PublishTeam`, no request (teams' `Rules.OnStart`)
Checked in this order:

| Case | Result |
|---|---|
| No team with that ID | Nothing published |
| Team not `IN_GAME` (lobby) | Nothing published |
| Team `IN_GAME` (always, when called by teams' `Start`) | `team_state` with the current state to every member: just after a start, the empty state (health 10, boss 1's clock from `started_at`); the attack timer is armed. Memory is unchanged except that an empty entry is created if the team had none (reads already report that same empty state); Postgres untouched |
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
| Team in the lobby or with no activity | `200`, empty lists, `selection` and `last_cast` null, `poisoned: false`, `bosses_defeated: 0`, `boss: {serial: 1, damage: 0, …}` (or `null` with no bosses loaded), `damage_multiplier: 1`, `health: 10` less any attacks since the start, `knocked_out: false`, `unlocked_points: 4`, `locked_shapes` (`[]` with the 2025 seed) |
| Knocked out | `200`, `health: 0`, `knocked_out: true`, `boss.next_attack_at: null` |
| Reads never catch up | Health is as of the last `catchUp`; the timer keeps it current, so a read can only lag a due attack by the timer's delay |
| Poisoned attempt | `200`, the stack with its wrong scan marked, `poisoned: true` |

### Boss attacks: the timer, and `catchUp`
| Case | Result |
|---|---|
| Before the first interval of the phase | No attack; `next_attack_at` = phase start + interval |
| Exactly at phase start + k × interval | The k-th attack is due (`floor` includes the boundary) |
| One attack due | `health` − the boss's damage, `attacks` + 1, `boss_attacked`; nothing written |
| Several due at once (late timer, a jump, downtime) | All applied as one change: one `boss_attacked` (or `knocked_out`) |
| Health reaches 0 | `health: 0`, `knocked_out: true`, `next_attack_at: null`, `knocked_out` event; no timer. `attacks` stays at the knockout attack, `ceil(PhaseHealth / attack_damage)`, however much time passes, and the same after a restart |
| Health would go below 0 | Floors at 0 |
| A boss's damage 0 (`game.boss_attack.damage`) | Attacks counted and published; health unchanged |
| Knocked out | Nothing attacks; no timer |
| Every boss defeated | Nothing attacks; no timer; `boss: null` |
| No bosses loaded | Nothing attacks |
| A timer fires early (nothing due) | No change, no event; re-armed |
| The store is closed (`Close`) | A running timer does nothing; nothing re-arms |

### Revive: `POST /api/admin/teams/{team_id}/revive`
Checked in this order:

| Case | Result |
|---|---|
| No token, a bad one, or the user gone | `401 missing_token` / `invalid_token` / `user_not_found` (admin middleware) |
| Caller isn't `ADMIN` in Postgres (whatever the token says) | `403 not_admin` |
| No team with that ID (any malformed ID too) | `404 team_not_found` |
| Team not `IN_GAME` | `409 not_in_game` |
| (any boss attack that fell due is applied first) | |
| Knocked out | `200`: health **10** (`game.health`), a new phase of the current boss from now, `attacks: 0`, timer re-armed; `team_boss` upserted; event `revived` |
| Not knocked out | The same: it also heals, and restarts the boss's clock |
| Every boss defeated | Health restored; nothing attacks |
| Postgres write fails | `500`; state reloaded; nothing applied |

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
- **An admin removing the leader versus the old leader's select or clear.** The same ordering, with `isLeader` read under the entry lock: a select that passed `isLeader` before teams applied the new leader counts; one checked after gets `403 not_leader` (and `404 not_in_team`, if the old leader was the one removed, from `isMember` first). The new leader can select from the moment the removal is applied.
- **The unlock versus a select or scan.** `unlocked_points` is derived from `State.Boss.Defeated` in the same snapshot the write validates against, under the entry lock: a scan racing the cast that defeats boss 5 runs either before it (the old limit) or after it (the new one), never half.

  So no write by a removed player is accepted once the removal is visible, and none is half-applied. Their earlier scans stay in the stack and still count toward a pattern (`TestAdminRemoveFromStartedTeam`, `TestAdminRemoveRacesScan`). After `RosterChanged`, publishes go to the remaining members only. Lock order is the same as the start hook's: teams entry → teamstate entry.
- **The attack timer versus writes.** The timer's callback (`attack`) takes `entry.mu` like a write, so an attack and a write never interleave. Whichever takes the lock first applies due attacks (`catchUp`); the other finds nothing more due. So every attack is applied exactly once, before any write that comes after its time, and its `boss_attacked` precedes that write's event. A timer delayed behind a held lock costs nothing: the write catches up.
- **A cast versus a due attack.** If an attack fell due before a cast but hadn't been applied, the cast's `catchUp` applies it first, so the cast's `deal` sees the real health and a defeat starts the next phase with it.
- **Timers and restarts.** Timers live in memory only. `Close` stops them (on shutdown, and in tests before a second store loads); `Load` re-arms every started team. Because attacks write nothing, even a stray timer of an old store could only change that store's memory, never Postgres.
- **The memory checks are the only enforcement** of the stack rules (one scan per waypoint per attempt, nothing after a wrong scan): they depend on the selection's `after_scan_id` window, which a unique constraint can't express. They all run under `entry.mu`, and Postgres has one writer (the instance lock in `platform/postgres`).

## Failure and recovery
| Case | Result |
|---|---|
| Postgres times out or drops during a write | `500`. `reconcile` re-reads the team (the write may or may not have committed) and memory matches Postgres |
| Memory and Postgres disagree (rows edited by hand) | The next write that notices (e.g. `deleteSelection` finding no row) fails, reconciles, and members get the corrected state |
| Postgres unreachable | Reads keep working from memory (the cast log included); writes fail |
| Backend restarts | Progress, selections, stacks (with their `wrong` flags, so `poisoned` too), the cast log and `last_cast`, the boss progress, multiplier and phase reload exactly. Health is recomputed from the phase and the clock: **attacks that fell due while the backend was down are applied at load**, as if it had stayed up (a team can come back knocked out). Timers are re-armed |
| `game.boss_attack.interval` or a boss's damage changed in `config.toml` between restarts | Applies retroactively to every current phase against that boss: health jumps to what the new values give from the phase start |
| `game.health` changed between restarts | A stored `phase_health` keeps its value; a `NULL` one (a team still in its first phase, never revived) uses the new value, from the team's start |
| `config.toml` changes between restarts | A larger `game.max_pattern_points` loads the same stacks. A smaller one refuses content with bigger patterns (`atlas.Load`); a stored stack longer than the new capacity loads only its newest scans. Don't change it mid-game |
| `game.bosses` or boss HP changed between restarts | See [`internal/bosses/README.md`](../bosses/README.md), "Failure and recovery". A team that had defeated more bosses than the new count refuses the start |
| Content edited while running | Not seen until restart (the atlas loads once). Don't: if the new content drops a waypoint, pattern or shape a team's stored state refers to (its cast log included), the next start refuses. See [`internal/atlas/README.md`](../atlas/README.md) |

## Limits and how to change them
| Limit | Where | To change |
|---|---|---|
| 6 points per pattern; stack capacity 7 | `config.toml` `game.max_pattern_points` (1–15) | Edit and restart. Content with bigger patterns is refused at startup (by `atlas.Load`) |
| 16 slots in a `Stack` | `config.MaxPatternPoints` (15) + 1 | Fixed array size; raise the constant to allow bigger patterns. Every snapshot grows by 56 bytes per slot |
| **6** bosses | `config.toml` `game.bosses` | Edit, match the `boss` table, restart ([`internal/bosses/README.md`](../bosses/README.md)) |
| Big patterns (**5**+ points, `game.locked_pattern_points`) locked until **5** bosses are defeated (`game.unlock_after_bosses`) | `config.toml` | Edit and restart. `locked_pattern_points` = `max_pattern_points` + 1 locks nothing; `unlock_after_bosses` = 0 unlocks from the start. A restart re-derives each team's unlock from `bosses_defeated`, so nothing is stored |
| Cast log length | none | Unbounded; 80 bytes per cast in memory, and a cast copies the team's log once |
| Starting health **10** | `config.toml` `game.health` (at least 1) | Edit and restart |
| Attack interval **10 min** | `config.toml` `game.boss_attack.interval` (positive; whole seconds, checked by `bosses.New`) | Edit, restart; applies retroactively (above) |
| Attack damage, by boss (**= serial**, 1…6) | `config.toml` `[game.boss_attack.damage]` (exactly serials 1…`game.bosses`, each ≥ 0) | Edit, restart; applies retroactively |
| One pending timer per started team | `time.AfterFunc` | ~100 bytes and no goroutine while waiting; 1,666 teams at 10,000 players is negligible |
| 64 waypoints, 64 shapes, 2048 patterns | the atlas's set types (`atlas.MaxWaypoints`, `atlas.MaxShapes`, `atlas.MaxPatterns`) | See [`internal/atlas/README.md`](../atlas/README.md), "Limits and how to change them". `State` holds one of each set, so a wider set makes every snapshot bigger |

## Not decided yet
- **⚠ The frontend play page doesn't scan QR yet.** The backend verifies the token (`POST …/scans` takes `{"qr"}`); the play page still submits a raw ID, so it gets `400 invalid_body` until it's wired to the camera scanner (`/test-scan` already reads tokens).
- **No big patterns in the 2025 seed.** Every seed pattern has 3 or 4 points, so the unlock changes nothing until content with 5- and 6-point patterns is loaded; the tests add some by SQL.
- **A non-leader can't restart a poisoned attempt.** Only the leader selects, so a team whose leader is away can't purge a wrong scan until the leader acts (or an admin removes the leader and names another).
- **Score and winning.** Damage now goes to bosses (`bosses_defeated`, `boss.damage`), but nothing ranks teams or marks one `FINISHED` after the last boss.
- **What sets the multiplier.** Stored and applied, never changed by any route yet.
- **Boss content changes mid-event apply retroactively.** Health is derived from the phase start with the boss's *current* interval and damage; storing them in the phase would freeze them.
- **What being knocked out costs.** Only the wait for an admin: progress, patterns and the cast log are kept, and a revive restores full health (and restarts the current boss's clock). No automatic revive, no penalty.
- **Clients see attacks no earlier than the server applies them.** `next_attack_at` is a countdown; the attack itself arrives as `boss_attacked` (at most the timer's delay later).

## Tests
- The content lookups teamstate relies on (`PatternFor`, `FitsShape`, `SetOf`, validation, the seed loading, `BenchmarkLookups`) are tested in the atlas: [`internal/atlas/README.md`](../atlas/README.md), "Tests".
- `tests/stack_test.go` (no database):

  | Test | Covers |
  |---|---|
  | `TestStack` | `NewStack(3)` has `Cap` 4 (a pattern's points plus one wrong scan); bottom-to-top order; a wrong scan on top keeps its flag; `Push` leaves the original alone; a push onto a full stack panics |

- `tests/store_test.go` (real Postgres, schema `teamstate_test`, the 2025 seed loaded through `atlas.Load` and `bosses.Load`, config.toml's rules including `game.health`, a fake clock). The fake `clock` implements `teamstate.Clock`: `Now` returns the fake time; `AfterFunc` records a timer; `at(d)` sets the time to `start` + `d` and then runs every timer now due, earliest first (never from inside `AfterFunc`, since the store calls it holding the team's lock); `pending()` counts live timers. Every helper (`scanAt`, `selectAt`) sets the clock first, so stored times are predictable. Times are written with `tick` (seconds). The fixture pins the team's `started_at` to `start` (and reloads teams), so boss 1's attacks fall at exactly `start` + k × interval. **Bosses don't attack unless a test asks** (`fixtureOptions.attacks`): otherwise `bossRules(set, attacks)` builds the fixture's `bosses.Rules` from `config.toml` (`game.bosses`, `[game.boss_attack]`) with the interval set to `neverAttack` (10 years), so tests that move the clock by minutes see no attacks. `fixtureOptions.bossRules` adjusts those rules for one test (an interval or a boss's damage). The tests use up to 6 members (`minTestTeam`), and the seed's 6 bosses (`seedBosses`); with a smaller `teams.size`, or another `game.bosses`, in `config.toml` they **skip** rather than fail:

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
  | `TestSixPointPatternAtCapacity` | a 6-point pattern (`game.max_pattern_points`) completes on the sixth scan (run with `unlock_after_bosses` 0, so big patterns are unlocked) |
  | `TestBigPatternsUnlock` | a 5-point pentagon and a shape mixing a 4- and a 5-point pattern (added by SQL; every boss at 1 HP): before 5 defeats `unlocked_points` is 4, the pentagon is in `locked_shapes` and selecting it is `ErrShapeLocked`, and in the mixed shape a point only in the 5-point pattern is a wrong scan; after `game.unlock_after_bosses` casts `unlocked_points` is `max_pattern_points`, nothing is locked, and the pentagon casts |
  | `TestOnlyLeaderSelects` | a member's select and clear are `ErrNotLeader` and change nothing; the leader's select works and members still scan |
  | `TestCompletingAgain` | `already_completed`, progress counted once |
  | `TestClearSelection` | clear with and without a selection; waypoints kept |
  | `TestRestartRestoresEverything` | restart reproduces the JSON view exactly, except `version`, which restarts at seq 0 in the new epoch; rescans still rejected |
  | `TestConcurrentScans` | two members at one waypoint (one accepted); one member at two waypoints (both accepted) |
  | `TestFailedWriteReconciles` | reload after a hand-edited database, published as `resync` one version on |
  | `TestVersionAndTypes` | (boss 1 given huge HP, so the cast stays a plain `spell_cast`) each publish's type (`game_started`, `shape_selected`, `scan_added`, `spell_cast`, `wrong_scan`, `selection_cleared`) and version: `PublishTeam` repeats it, every change adds one, a rejected scan and a no-op clear publish nothing and keep it; `cast_count` 0 until the cast, then 1; `Snapshot` is of type `snapshot` |
  | `TestReadsDoNotTouchDatabase` | reads with the database closed; a failed write leaves memory alone |
  | `TestStateIsPublished` | live events reach teammates |
  | `TestPublishTeam` | `PublishTeam` sends the empty state to every member of a started team; after a selection it sends the current state (square selected) without changing it; nothing for an unknown team or a lobby team |
  | `TestCastLog` | walkthrough 6: no casts before; `last_cast` (pattern, shape, `cast_by`, `cast_at`, `damage`) after each of two casts, the repeat marked `already_completed`; a new selection keeps them; `CastsForUser` newest first; two `team_cast` rows; a restart reloads the same log (JSON equal) |

- `tests/boss_test.go` (same fixture; `castSquare` selects and scans the seed square, `restart` closes the store and loads a new one from the same database):

  | Test | Covers |
  |---|---|
  | `TestCastsDamageBosses` | walkthrough 7 against a model built from the seed's HPs: casts until two bosses fall, checking after each `dealt`, `boss`, `defeated_boss`, `boss.serial`/`damage`, `bosses_defeated`, the event type (`spell_cast`, then `boss_defeated` on a defeat), the excess lost; `team_boss` in Postgres; a restart reproduces the view (except `version`) and the cast log |
  | `TestDamageMultiplier` | `team_boss.multiplier = 1.5` set in Postgres, restart: `damage_multiplier` 1.5; a cast's `dealt` is `round(damage × 1.5)` while `damage` is the pattern's; the multiplier survives the cast's upsert and another restart |
  | `TestEveryBossDefeated` | every boss at 1 HP: each cast defeats one; after the last, `boss` null and `bosses_defeated` = the count; one more cast counts (`cast_count`) but deals 0 and hits no boss |
  | `TestLoadRefusesTooManyDefeated` | a `team_boss` row with `defeated` above `game.bosses` stops `Load` |

- `tests/attack_test.go` (same fixture with `attacks: true`; `expectType` reads the next event's type, `expectNoEvent` fails on a waiting one):

  | Test | Covers |
  |---|---|
  | `TestBossAttacks` | walkthrough 8: at the start full health, `attacks` 0, `phase_started_at` = start, `next_attack_at` one interval on, one timer pending; nothing 1 µs before the first interval; exactly at it `boss_attacked`, health − damage, the next one interval later; defeating boss 1 starts boss 2's phase at the cast with the health then; boss 2's attacks one interval at a time, then `knocked_out` (health 0, `next_attack_at` null, no timer); no attacks after; `Select` and `Scan` get `ErrKnockedOut`, `ClearSelection` works; `Revive`: full health, `attacks` 0, phase from now, one timer, `revived`; selecting works again |
  | `TestAttacksAcrossRestart` | `Close` stops every timer; with the store closed the clock passes three attacks and memory doesn't change; a restart applies all three at load (health, `attacks`, `next_attack_at`); after a revive and one more attack, a restart reproduces the view exactly (JSON, version aside) |
  | `TestWriteCatchesUpAttacks` | boss 1's damage set to 4 through the rules (`fixtureOptions.bossRules`); with the timers off (`Close`), a scan after the first attack publishes `boss_attacked` then `scan_added` and sees the lower health; after a knockout's worth of attacks a select is refused with `ErrKnockedOut` and publishes `knocked_out` |
  | `TestReviveErrors` | `Revive` of an unknown team → `ErrTeamNotFound`; of a lobby team → `ErrNotInGame` |

- `tests/gameloop_test.go`: **the game-loop simulation**, its own fixture (`newLeague`, schema `teamstate_sim`): the 2025 seed with its boss attacks, **100** teams (`simTeams`) of every size from 1 to `teams.size`, all started at the fake clock's start, the teams store's `OnRosterChange` wired to `RosterChanged`, plus a 5-point **pentagon** and a 6-point **hexagon** (`simBigPatterns`, each its own shape: the seed has no big patterns). One pick in five is a big pattern (`randomPattern`), so every team tries them before and after its unlock: before, `lockedFor` expects `shape_locked` (or `knocked_out`, checked first). Every select and clear is made by the leader (the first active member: member 1 once an admin removed member 0). Team *i* plays strategy *i* mod 12 at size 1 + (*i* / 12) mod 6, so each strategy runs at several sizes:

  | Strategy | What it does |
  |---|---|
  | `steady` | casts a random pattern every 2 steps, members taking turns |
  | `rush` | casts every step: defeats every boss, then casts for nothing |
  | `solo` | one member scans every point, every 3 steps |
  | `idle` | never plays: knocked out by boss 1 (10 attacks × 5 min), revived at step 110, attacked again |
  | `sloppy` | a wrong scan (`wrong_scan`, `poisoned`), a refused scan (`selection_poisoned`), a reselect, a cast |
  | `retrier` | sends every scan twice: the resend gets `waypoint_already_scanned` (`no_shape_selected` after the completing one) |
  | `quitter` | gives up half its attempts after one scan (`ClearSelection`: no selection, empty stack) |
  | `repeater` | casts the same pattern every time: `already_completed` after the first |
  | `slowpoke` | casts every 8 steps: attacked mid-fight, knocked out, revived 3 steps later |
  | `racer` | two members (one member twice, for a team of one) scan the same point at once: exactly one accepted |
  | `shrinking` | an admin removes a member at step 40 (the leader, with member 1 as the new leader, for even teams), or is refused the last member (`ErrLastMember`); the removed member's scans get `ErrNotInTeam` |
  | `multiplier` | a stored multiplier (×2, ×1.5, ×0.5, ×3) scales every cast |

  | Test | Covers |
  |---|---|
  | `TestGameLoopSimulation` | **180 steps of 30 s** (90 min of game time). Each step moves the fake clock (firing every due attack), then all 100 teams act at once by strategy, every call's error checked. After each step every team's state is compared with **`model`**, an independent re-implementation of the rules: `bosses_defeated`, `boss` (serial, damage, `attacks`, `phase_started_at`, `next_attack_at`), `health`, `knocked_out`, `damage_multiplier`, `cast_count`, `unlocked_points` and whether the pentagon and hexagon are in `locked_shapes` (`model.unlocked`); and each cast's `dealt`, `boss`, `defeated_boss`, `already_completed`. At the end, every team's watcher (a member never removed, subscribed to the hub) must hold the team's state as its last `team_state`, version included (`converge`). Counters fail the test if any configuration never happened. The league's bosses attack every **`simAttackInterval` (5 min)** with `config.toml`'s damages (shorter than the configured 10 min, so 90 minutes see many attacks and knockouts, yet the fastest teams still get through every boss). Last run (race detector, ~3 s): 2 teams through every boss, 131 knockouts, 48 revives, and every other counter above zero (run with `-v` for all of them). It found the knockout-cap bug ("Boss attacks and health") |
  | `TestGameLoopRestart` | The same league, backend down part way: **45 steps**, then the store waits for every event (`converge`), is closed (no timers fire), the clock moves on by the downtime, and a new store loads in a new epoch; then **45 more steps**. Subtests per downtime: **0** (every view must survive exactly, JSON with version aside), **90 s** (shorter than the league's attack interval, `simAttackInterval`), **15 min** (several attacks of every boss), **1 h** (must knock teams out while down). After the restart every team must equal its `model` at the new time, so the attacks that fell due while down are applied at load, knockouts included; each watcher gets the new store's `Snapshot`, as a reconnecting phone would, and every phone must converge at the end. Last run (race detector, ~10 s for all four): knocked out while down 0, 16, 42 and 51 (90 s can knock out a team at its last health point with an attack due) |
  | `BenchmarkGameLoopCast` | casts per second with the 100 teams playing at once, boss attacks off: each goroutine owns a team and casts random patterns (a select plus every scan, each a Postgres write). **433,873 ns per cast** (`-benchtime 3s`, 16 CPUs, local Postgres 17), about 2,300 casts or 11,000 writes a second |

- `tests/publish_bench_test.go`: **`BenchmarkTeamStatePublish`**, parallel `PublishTeam` calls across 1,666 `IN_GAME` teams (10,000 players at `teams.size` 6; an in-memory `Teams`, so no team rows), every member with an open subscription. It measures the lock, encoding once and queueing for each member: about **1.0 µs** a publish on a 10-core laptop, down from 4.4 µs before the sharded hub and roster cache (`internal/platform/README.md`, "Performance"). Measured before the cast log and `poisoned` were added to the `View`; a team with no casts encodes the same size plus `"poisoned":false`.
- `internal/server/tests/teamstate_test.go`:

  | Test | Covers |
  |---|---|
  | `TestSelectAndScanRoutes` | every route and error code over HTTP: `not_in_game`, `not_in_team`, `invalid_body` (the body size limit included), `invalid_qr` (tampered) and `qr_expired` (stale) for a scan, `unknown_shape`, `unknown_waypoint`, `no_shape_selected`, `waypoint_already_scanned`; one member scanning two waypoints; a square completing; a wrong scan (`wrong_scan`, `poisoned`, `stack[].wrong`), then `selection_poisoned`, then a reselect clearing it. Scans submit tokens minted with the test secret (`qrToken`) |
  | `TestCastLogRoute` | `GET /api/teams/me/casts`: `404 not_in_team`, `[]` before any cast, then two casts newest first with `already_completed`, `shape` and `damage` |
  | `TestGameBosses` | `GET /api/game/bosses`: `401 missing_token`; `[]` before content; after seeding, `game.bosses` bosses in serial order with name, asset and HP; a started team's state shows boss 1 at 0 damage, multiplier 1; after a square over HTTP, `last_cast` hit boss 1 with `dealt` = `damage` and `boss.damage` = `dealt` |
  | `TestGameShapes` | `GET /api/game/shapes`: `401 missing_token` without a token; the seed's 5 shapes, `square` with 18 patterns of 4 points, a valid damage range per shape, `max_pattern_points` from `config.toml`, and no `scan_window_ms` |

- `internal/server/tests/simulation_test.go`: simulated phones, each with its own request and response delays and its own event stream delivered after that delay:

  | Test | Covers |
  |---|---|
  | `TestSimTeamCompletesUnderLatency` | four players scan the square at once with delays up to 150 ms each way: exactly one `pattern_completed`; every screen ends with an empty stack and the square completed |
  | `TestSimRaceSameWaypoint` | two players scan the same waypoint at once: one `200`, one `409 waypoint_already_scanned`; every screen converges on a one-scan stack |
  | `TestSimWrongScanReachesEveryone` | a wrong scan racing a teammate's correct one: the wrong one gets `wrong_scan`, the other is accepted or gets `selection_poisoned`; every phone sees `poisoned`; a reselect clears it on every phone |
  | `TestSim100Teams` | **100 teams** of every size at once over real HTTP and event streams, each player on its own delays (0–30 ms each way), 6 rounds each by one of 7 strategies (`steady`, `solo`, `sloppy` with waypoint `1012`, `racer`, `retrier`, `quitter`, `shrinking` with an admin removal over HTTP); bosses never attack. Every request's status and code is checked; at the end each team's `cast_count` is the casts it made, every phone holds the team's state (same `version`, kept newest-by-version like the frontend), and every removed player got `no_team` |

- `internal/server/tests/admin_test.go`:

  | Test | Covers |
  |---|---|
  | `TestAdminRemoveFromStartedTeam` | after an admin removes a member of a started team: their stream gets `no_team` and no further `team_state`; their scan, selection and state read get `404 not_in_team`; the rest complete a square using the removed player's earlier scan |
  | `TestAdminRemoveRacesScan` | a member's scan racing their removal, for every non-leader: the removal always succeeds; the scan is accepted (and stays in the stack) or refused with `not_in_team`, never accepted after the removal. Which wins isn't controlled; the refused path is covered deterministically above |
  | `TestAdminRevive` | a started team's state has `health` = `game.health`, `knocked_out` false and a `next_attack_at`; a stored phase with 0 health, applied at restart, knocks it out (`health` 0, `next_attack_at` null); select and scan get `409 knocked_out`; the revive route: `403 not_admin` for a player, `404 team_not_found`, `409 not_in_game` for a lobby team; an admin's revive returns full health and `attacks` 0, a member's stream gets `revived`, and selecting works again |

- `internal/server/tests/events_test.go`: `team_state` on connect; `TestStartPublishesTeamState`: when the leader starts, every open stream gets `team` then the empty `team_state`, in that order.
