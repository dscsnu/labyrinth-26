# bosses

The sequence of bosses every team fights, in serial order: each boss's name, image asset, description, HP, and how often and how hard it attacks. Loaded from Postgres once at startup, validated against `config.toml`, held in memory, and never changed while the backend runs. A **read-only use case** with one route, `GET /api/game/bosses`. A team's progress through the bosses (which boss, how much damage, the multiplier) and its health under their attacks are per-team state and belong to `teamstate` (see its README, "Bosses" and "Boss attacks and health").

Every number below that comes from `config.toml` is shown with its current value and its key, e.g. **7** (`game.bosses`). Change the file, not the code.

## Contents
1. [Concepts](#concepts)
2. [The flow](#the-flow)
3. [How each file works](#how-each-file-works)
4. [Data model: memory](#data-model-memory)
5. [Data model: Postgres](#data-model-postgres)
6. [Memory ↔ Postgres](#memory--postgres)
7. [Damage rules](#damage-rules)
8. [Attack rules](#attack-rules)
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
| **Boss** | One opponent: `serial`, `name`, `asset`, `description`, `hp`, `attack_interval_s`, `attack_damage`. Static content in the `boss` table. |
| **Serial** | A boss's place in the sequence, from 1. Every team fights serial 1 first, then 2, and so on. Serials are **dense**: exactly 1…**7** (`game.bosses`). |
| **HP** | The damage a team must deal to defeat the boss. Always > 0. |
| **Attack** | Every `attack_interval_s` seconds of a team's **phase** against the boss, it takes `attack_damage` off the team's health. Interval > 0; damage ≥ 0 (0: it attacks harmlessly). Applied by `teamstate`. |
| **Phase** | The span a team spends on one boss: from its start (boss 1) or the cast that defeated the previous boss, or an admin's revive, until the boss is defeated. The attack clock counts from the phase's start. |
| **Health** | Per team, starts at **10** (`game.health`); boss attacks lower it; at 0 the team is **knocked out**. Owned by `teamstate`. |
| **Asset** | A key the frontend resolves to the boss's image, e.g. `bosses/1.png` (served from `frontend/static/`). Not a URL. |
| **Bosses count** | `game.bosses`, **7**: how many bosses every team fights. The table must hold exactly that many, or none at all. |
| **Progress** | A team's place in the sequence: bosses defeated, damage dealt to the current one, and its damage multiplier. Owned by `teamstate` (`State.Boss`, table `team_boss`), not here. |
| **Dealt** | What one cast does to the current boss: the pattern's damage × the team's multiplier, rounded. Computed by `teamstate`. |
| **Multiplier** | Per team, default **1**. Nothing changes it yet; it's stored so a future power-up can. |

## The flow
1. **Content is loaded.** `go run . seed` (the 2025 dev seed) writes 7 placeholder bosses; real content would be a migration or a seed of its own. The backend is stopped while content changes.
2. **Startup.** `server.Build` calls `bosses.Load` right after `atlas.Load`, with `Rules{Count: game.bosses}`. It reads the table in serial order and runs `New`'s checks. Any bad content stops the backend.
3. **teamstate loads** with the `*Bosses` as its `Bosses` interface, and refuses a team whose stored progress has more bosses defeated than exist.
4. **Clients fetch the list once.** `GET /api/game/bosses` returns all bosses in serial order. Each team's `team_state` carries only its progress (`boss: {serial, damage, phase_started_at, attacks, next_attack_at}`, `bosses_defeated`, `damage_multiplier`, `health`, `knocked_out`); the client joins the two by serial.
5. **Every cast** (a completed pattern) damages the team's current boss: see [Damage rules](#damage-rules).
6. **The boss attacks back** on a clock: see [Attack rules](#attack-rules).

**Why a separate use case and not part of `atlas`.** The atlas is the map (waypoints, shapes, patterns, spells). Bosses are a different kind of content with their own table, route and config key; keeping them apart keeps each package small, and teamstate reads each through its own interface (AGENTS.md §4).

**Why the progress isn't here.** Per-team gameplay results go in `team_state` (AGENTS.md §17), so a cast changes the state and its boss progress in one write, under one lock, published as one event.

## How each file works
### `doc.go`
The package comment.

### `read.go`: types and lookups
- **`Boss`**: one boss, with JSON tags (it's also the API's shape): `serial`, `name`, `asset`, `description`, `hp`, `attack_interval_s`, `attack_damage`.
- **`AttackInterval()`**: `AttackIntervalS` as a `time.Duration` (seconds), for teamstate's attack clock.
- **`Bosses`**: the in-memory sequence (`list []Boss`).
- **`Count()`**: how many bosses: `game.bosses`, or 0 if none are stored.
- **`At(i)`**: the boss at index `i` (serial `i+1`). Panics on an out-of-range index: callers index by a team's progress, which teamstate never lets pass `Count()`.
- **`All()`**: every boss in serial order, for the route. Returns the store's own slice; callers must not modify it.

### `store.go`: layout, `Rules`, `Load`, validation
- **`Rules{Count}`**: `game.bosses`.
- **`Load(ctx, pg, log, rules)`**: `selectBosses`, then `New`. Wraps errors as `load bosses: …` (query) or `invalid bosses: …` (content). Logs a warning when the table is empty, then `loaded bosses` with the count.
- **`New(list, rules)`**: validates a list already in serial order and builds the `Bosses`. Used by `Load` and directly by tests. Checks are in [Rules and cases](#loading-at-startup).

### `db.go`: SQL (reads only)
- **`selectBosses`**: `SELECT serial, name, asset, description, hp, attack_interval_s, attack_damage FROM boss ORDER BY serial`. Returns an empty (non-nil) slice for an empty table.

### `api.go`: the route
- **`NewAPI(b)`**, **`Routes(mux, authed)`**: registers `GET /api/game/bosses` behind the auth middleware.
- **`all`**: `httpx.OK(w, bosses.All())`.

### `tests/store_test.go`
See [Tests](#tests).

## Data model: memory
```
Bosses
  list  []Boss   index → boss, where index = serial − 1

Boss (80 bytes, plus its strings' bytes)
  Serial          int
  Name            string
  Asset           string
  Description     string
  HP              int
  AttackIntervalS int   seconds between attacks
  AttackDamage    int   health each attack takes
```
- **Why a slice and no map.** Serials are dense (exactly 1…`game.bosses`, checked by `New`), so the serial itself is the key: boss `n` is `list[n-1]`. A lookup is one slice index, O(1), with no hashing, no map overhead and no possible miss. A team's progress is an index too (`Defeated` = the index of the current boss), so teamstate never converts.
- **Size:** 7 bosses × 80 bytes, plus about 600 bytes of strings in the seed: under 1.2 KB, once for the whole process.
- Built once by `New`; never modified, so no locks.

## Data model: Postgres
Migrations `0009_bosses.sql` (the table) and `0010_boss_attacks.sql` (the attack columns).

**`boss`** (read here):

| Column | Type | Constraint | Rule it backs |
|---|---|---|---|
| `serial` | `INTEGER` | `PRIMARY KEY`, `CHECK (serial >= 1)` | One boss per place; serials start at 1. Density (no gaps) is checked by `New`, since a constraint can't see other rows |
| `name` | `TEXT NOT NULL` | `CHECK (char_length(name) BETWEEN 1 AND 64)` | A name to display |
| `asset` | `TEXT NOT NULL` | `CHECK (asset <> '')` | An image key |
| `description` | `TEXT NOT NULL` | | May be empty |
| `hp` | `INTEGER NOT NULL` | `CHECK (hp > 0)` | A boss can be defeated, and isn't defeated before it's hit |
| `attack_interval_s` | `INTEGER NOT NULL` (0010) | `CHECK (attack_interval_s > 0)` | A boss attacks on a clock; a zero interval would attack endlessly |
| `attack_damage` | `INTEGER NOT NULL` (0010) | `CHECK (attack_damage >= 0)` | An attack never heals |

0010 added the attack columns with defaults (300, 1) to fill existing rows, then dropped the defaults: a new boss must give both.

0009 also adds `team_boss` and three `team_cast` columns, and 0010 adds `team_boss.phase_health` and `phase_started_at`; those belong to teamstate: see its README.

**Seed** `seeds/dev_2025_bosses.sql`, applied last by `SeedDev2025` (after `dev_2025.sql` and `dev_2025_spells.sql`). Idempotent: `INSERT … ON CONFLICT (serial) DO UPDATE`. The 2025 theme's seven evil exes:

| Serial | Name | Asset | HP | Attacks every | Attack damage |
|---|---|---|---|---|---|
| 1 | Matthew Patel | `bosses/1.png` | 200 | 300 s (5 min) | 1 |
| 2 | Lucas Lee | `bosses/2.png` | 300 | 300 s (5 min) | 1 |
| 3 | Todd Ingram | `bosses/3.png` | 400 | 270 s (4.5 min) | 1 |
| 4 | Roxy Richter | `bosses/4.png` | 500 | 240 s (4 min) | 1 |
| 5 | Kyle Katayanagi | `bosses/5.png` | 600 | 240 s (4 min) | 1 |
| 6 | Ken Katayanagi | `bosses/6.png` | 700 | 180 s (3 min) | 2 |
| 7 | Gideon Graves | `bosses/7.png` | 1000 | 120 s (2 min) | 2 |

Seed casts deal 60–131 at multiplier 1, so boss 1 takes two to four casts and boss 7 eight to seventeen. 3,700 HP in all. Attacks get faster and harder: at full health (**10**, `game.health`) a team stalled on boss 1 is knocked out after 50 minutes (10 attacks × 5 min), on boss 7 after 10 minutes (5 attacks × 2 min).

## Memory ↔ Postgres
| Postgres | Memory |
|---|---|
| `boss` rows, `ORDER BY serial` | `Bosses.list`, index = serial − 1 |

Nothing is written at runtime, so a restart rebuilds the list exactly from the same rows.

## Damage rules
Applied by `teamstate` (`deal` in `read.go`, called by `Scan` when a scan completes a pattern). Stated here because they define what a boss is for.

1. A team fights the boss at index `Defeated` (serial `Defeated+1`), starting at boss 1 with 0 damage and multiplier 1.
2. Each cast deals **`round(pattern damage × multiplier)`** (`math.Round`: halves round away from zero, e.g. 107 × 1.5 = 160.5 → **161**).
3. Damage adds to the current boss's total. When the total reaches the boss's **HP** (≥), the boss is defeated: `Defeated` goes up by one and the total resets to 0.
4. **Overkill is lost.** A cast never carries damage over to the next boss: a cast of 107 on a boss with 7 HP left defeats it, and the next boss starts at 0.
5. **One cast defeats at most one boss**, by rule 4.
6. **After the last boss**, `Defeated` = `game.bosses`: `boss` is `null` in `team_state`, and casts still count toward patterns, shapes and the cast log but deal **0** and hit no boss (`dealt: 0`, `boss: null`).
7. **The multiplier** is per team, stored in `team_boss.multiplier`, default 1. **Nothing sets it yet**: it's there for a future power-up. Casts never change it.
8. Every cast records what it did: `dealt`, the boss's serial (`boss`), and `defeated_boss`.
9. A team's `team_state` event for a cast that defeats a boss has type **`boss_defeated`** instead of `spell_cast`.
10. **A defeat starts the next boss's phase** at the cast's time, with the health the team has then: the next boss's attack clock starts from zero.

## Attack rules
Applied by `teamstate` (`BossProgress.at`, `catchUp`, the attack timer). Health is **derived, never stored per attack**:

1. A team's phase against its current boss starts at `PhaseStart` with `PhaseHealth`: boss 1 at the team's start (`team.started_at`) with full health (**10**, `game.health`); boss n+1 at the cast that defeated boss n, with the health then; or at an admin's revive, with full health.
2. At any time `now`: **`attacks = floor((now − PhaseStart) / attack_interval_s)`** and **`health = max(0, PhaseHealth − attacks × attack_damage)`**. The k-th attack is at exactly `PhaseStart + k × attack_interval_s` (k = 1, 2, …).
3. **Health reaching 0 knocks the team out.** Selecting and scanning are refused (`409 knocked_out`), clearing a selection still works, and the boss stops attacking (no timer, `next_attack_at: null`).
4. **An admin revives** a started team (`POST /api/admin/teams/{team_id}/revive`, any time, so it also heals): health back to 10, and a new phase of the same boss from now (its attacks so far no longer count).
5. **After the last boss**, nothing attacks.
6. Only phase boundaries are stored (`team_boss.phase_started_at`, `phase_health`: a defeat or a revive). Attacks themselves are memory-only: a restart recomputes them from the phase and the clock, including any that fell due while the backend was down.
7. An `attack_damage` of 0 still counts attacks (and publishes them) but never lowers health.

## Walkthroughs
### 1. Loading the seed
`game.bosses` = 7. `selectBosses` returns 7 rows, serials 1…7. `New` checks count (7 = 7), then each row: serial = index+1, name, asset, HP > 0. `list[0]` is Matthew Patel (200 HP). Log: `loaded bosses bosses=7`.

### 2. A team fights boss 1 (square, damage 107, multiplier 1)
| Cast | Before | Dealt | After | `last_cast` | Event type |
|---|---|---|---|---|---|
| 1 | boss 1, 0/200 | 107 | boss 1, 107/200 | `dealt 107, boss 1, defeated_boss false` | `spell_cast` |
| 2 | boss 1, 107/200 | 107 | 214 ≥ 200: boss 2, 0/300, `bosses_defeated 1` | `dealt 107, boss 1, defeated_boss true` | `boss_defeated` |

The 14 points past 200 are lost (rule 4). `team_boss` after cast 2: `defeated 1, damage 0, multiplier 1`.

### 3. Multiplier 1.5
An admin (by SQL, for now) sets `team_boss.multiplier = 1.5`, then the backend restarts. The square now deals `round(107 × 1.5)` = **161**; `last_cast.damage` is still 107, `last_cast.dealt` is 161. The cast's upsert writes `defeated` and `damage` only, so the multiplier stays 1.5.

### 4. Every boss defeated
After the seventh defeat: `bosses_defeated 7`, `boss null`. The next cast: pattern counted, `cast_count` up, `dealt 0`, `boss null`, `defeated_boss false`, type `spell_cast`.

### 5. Boss attacks (seed, `game.health` 10)
A team starts at 12:00:00. Boss 1 attacks every 300 s for 1: at 12:05:00 health is 9 (`boss.attacks` 1, `next_attack_at` 12:10:00); at 12:10:00, 8. A cast at 12:12:30 defeats boss 1: boss 2's phase starts at 12:12:30 with 8 health, its first attack at 12:17:30. If the team then stalls, boss 2 (300 s, 1) knocks it out 8 attacks later, at 12:52:30. An admin revives it at 13:00:00: health 10, boss 2's first attack of the new phase at 13:05:00.

### 6. A refused start
`game.bosses` is lowered from 7 to 6 with 7 rows stored: `invalid bosses: 7 bosses stored; game.bosses is 6 (the boss table must be exactly serials 1-6)`, exit 1.

## The write-through
Not applicable: this use case never writes. Content changes through migrations and seeds with the backend stopped. Team progress is written by teamstate (`team_boss`, `team_cast`) in the same transaction as the cast.

## API
| Method | Path | Auth | Response |
|---|---|---|---|
| GET | `/api/game/bosses` | Bearer | Every boss, serial 1 first |

```json
{
  "success": true,
  "payload": [
    {"serial": 1, "name": "Matthew Patel", "asset": "bosses/1.png", "description": "The first evil ex. …", "hp": 200, "attack_interval_s": 300, "attack_damage": 1},
    {"serial": 2, "name": "Lucas Lee", "asset": "bosses/2.png", "description": "…", "hp": 300, "attack_interval_s": 300, "attack_damage": 1}
  ]
}
```
- The same for every signed-in user, static while the backend runs; fetch it once (`/play` loads it with the page).
- `[]` before boss content is loaded.
- Errors: `401 missing_token`, `401 invalid_token` (auth middleware).
- No live events: the list never changes while running. A team's progress arrives in `team_state`.

## Rules and cases
### Loading at startup
`Load` returns an error and the backend refuses to start, checked in this order (the first found is reported):

| # | Case | Result |
|---|---|---|
| 1 | The query fails (database unreachable, table missing) | `load bosses: …` |
| 2 | Table empty | **Allowed**, like an empty atlas, so a fresh database boots. `Count()` = 0; warning `no bosses stored: casts hit no boss until boss content is loaded and the backend restarted`. Every team's `boss` is `null` and casts deal 0 |
| 3 | Row count ≠ `game.bosses` (fewer or more) | `invalid bosses: N bosses stored; game.bosses is M (the boss table must be exactly serials 1-M)` |
| 4 | A gap, a duplicate, or not starting at 1 (row `i` doesn't have serial `i+1`) | `invalid bosses: boss serial S where E was expected: serials must be 1-M with no gaps` |
| 5 | Empty name | `invalid bosses: boss S has no name` |
| 6 | Empty asset | `invalid bosses: boss S has no asset` |
| 7 | HP ≤ 0 | `invalid bosses: boss S: hp must be positive, got H` |
| 8 | `attack_interval_s` ≤ 0 | `invalid bosses: boss S: attack_interval_s must be positive, got N` |
| 9 | `attack_damage` < 0 | `invalid bosses: boss S: attack_damage must not be negative, got N` |

Cases 5–9 are also enforced by Postgres CHECKs; `New` repeats them for fixtures built in tests. Duplicate serials can't be stored (primary key) but `New` would catch them as case 4.

teamstate adds one more startup refusal: a `team_boss` row with `defeated` greater than `Count()` (`team_boss: N bosses defeated, but game.bosses is M`).

### `config.toml`
| Case | Result |
|---|---|
| `game.bosses` missing | `missing key game.bosses` |
| `game.bosses = 0` | `game.bosses must be at least 1` |

### Lookups
| Call | Result |
|---|---|
| `At(i)`, 0 ≤ i < `Count()` | the boss with serial `i+1` |
| `At(i)` out of range | panics (a bug: teamstate only indexes below `Count()`) |

## Concurrency and locking
No races: the list is built before `Load` returns and never modified, so every goroutine reads it without a lock. `All()` returns the shared slice; callers must not modify it.

## Failure and recovery
| Case | Result |
|---|---|
| Bad content | `server.Build` returns the error; exit 1 before serving. Fix the table (with the backend stopped), restart |
| Postgres unreachable at startup | `load bosses: …`; exit 1 |
| Postgres down while serving | No effect: nothing here reads Postgres after `Load` |
| Content edited while running | Not seen until restart |
| A boss's HP lowered between restarts below a team's stored damage | Loads. The team's next cast reaches the HP and defeats it (the extra is lost) |
| A boss's `attack_interval_s` or `attack_damage` changed between restarts | Applies **retroactively** to every team's current phase against it: health is recomputed from the phase start with the new values, so it can jump (down or up) at the restart |
| `game.bosses` raised (with new rows) | Teams that had defeated every boss now have one more to fight (`boss` reappears) |
| `game.bosses` lowered below a team's `defeated` | teamstate refuses to start; fix `team_boss` or the config |
| Restart | The list is rebuilt exactly |

## Limits and how to change them
| Limit | Where | To change |
|---|---|---|
| **7** bosses | `config.toml` `game.bosses` (at least 1) | Edit, add or remove `boss` rows to match, restart |
| Boss name 1–64 characters | `boss.name` CHECK | A migration |
| HP an `INTEGER` | `boss.hp` | A migration (bigint) if HP ever passes 2^31 |
| Attack interval in whole seconds, at most 2^31 − 1 (about 68 years) | `boss.attack_interval_s` | A migration |
| Starting health **10** | `config.toml` `game.health` (at least 1) | Edit, restart; stored phases keep their `phase_health` |

## Not decided yet
- **What sets the multiplier.** Only stored and applied; no route or rule changes it. A power-up use case would update `team_boss.multiplier` and teamstate's memory together (a teamstate write, AGENTS.md §6).
- **Overkill carry-over.** Lost by design for now; carrying it to the next boss is a one-line change in `deal`.
- **Winning.** Defeating every boss doesn't change the team's status (`FINISHED` is still unset) or rank teams; a leaderboard would read `bosses_defeated` and the last `defeated_boss` cast's time.
- **Content changes mid-event.** A changed attack interval or damage applies retroactively to every current phase after a restart (health is derived). If that's ever unwanted, the phase would have to store the boss's interval and damage too.
- **What knocked out costs beyond the wait.** Nothing else yet: progress, patterns and casts are kept; only an admin's revive lets the team play again.
- **Per-team boss order.** The 2025 rules gave each team a random order of the first six bosses. Everyone fights in serial order now.
- **Images.** No files exist in `frontend/static/bosses/` yet; `/play` hides a missing image.

## Tests
`tests/store_test.go`:

| Test | Covers |
|---|---|
| `TestNew` | a valid three-boss list: `Count`, `At`, `All`, `AttackInterval`, attack damage; an empty list loads with `Count` 0; each refusal by its message: too few, too many, zero attack interval, negative attack damage, a gap, starting at 0, no name, no asset, zero HP, negative HP, a duplicate serial |
| `TestLoad` | an empty table loads (`Count` 0); the 2025 seed with `game.bosses`: every boss in serial order with name, asset, positive HP, a positive attack interval and non-negative attack damage, HP rising; one fewer in config is refused. Skips unless `game.bosses` is the seed's **7** (`seedBosses`) |

Boss damage and attacks are tested in teamstate (`tests/boss_test.go`, `tests/attack_test.go`) and over HTTP in `internal/server/tests` (`TestGameBosses`, `TestAdminRevive`).
