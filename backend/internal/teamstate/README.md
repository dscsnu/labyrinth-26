# teamstate

What each team does in the game: the shape it has selected, its stack of scans toward a pattern of that shape, the waypoints it has visited, the patterns and shapes it has completed, its cast log, its progress through the bosses, and its health under their attacks. There is **no time limit** on an attempt. Three changes are caused by time passing, all published by the server with nobody acting: a **boss attack** (every boss attacks on a clock, lowering the team's health), a **QR cooldown ending** (a cast's waypoints are disabled for the team for **5 min**, `game.qr_cooldown`, then open again) and a **powerup window ending** (a `short_qr_cooldown` powerup's). Teams also hold **powerups**, temporary effects their leader uses.

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
10. [QR cooldown](#qr-cooldown)
11. [Powerups](#powerups)
12. [Walkthroughs](#walkthroughs)
13. [The write-through](#the-write-through)
14. [API](#api)
15. [Rules and cases](#rules-and-cases)
16. [Concurrency and locking](#concurrency-and-locking)
17. [Failure and recovery](#failure-and-recovery)
18. [Limits and how to change them](#limits-and-how-to-change-them)
19. [Not decided yet](#not-decided-yet)
20. [Tests](#tests)

The waypoints, shapes and patterns themselves are static content owned by the **atlas** use case: how they're loaded, validated and stored (indexes, bitmasks, `FitsShapeCastable`) is in [`internal/atlas/README.md`](../atlas/README.md). The bosses (name, HP, asset, attack interval and damage) are static content owned by the **bosses** use case ([`internal/bosses/README.md`](../bosses/README.md)). This README covers what teams do with them: a team's progress through the bosses, and its health, are part of its `State`.

## Concepts
| Term | Meaning |
|---|---|
| **Atlas** | The static content (waypoints, shapes, patterns), loaded once at startup by the atlas use case and read here through the [`Atlas` interface](#storego-the-in-memory-layout). Never changes while the backend runs. |
| **Waypoint** | A physical location with a QR code. Identified by a **4-digit ID** (`"2001"`; leading zeros allowed). |
| **QR token** | The signed, rotating string a waypoint's QR shows (`"<waypoint>:<unix seconds>.<signature>"`). A scan submits it instead of the raw ID. Owned by the [`qr`](../qr/README.md) use case; verified here through the `QR` interface before the waypoint is resolved. |
| **Pattern** | A set of **1 to 6** (`game.max_pattern_points`) waypoints that forms a shape on the map. Identified **by that set**, e.g. `["2001","2002","2007","2008"]`, in any order. Its database `id` is only an internal key. |
| **Shape** | What a pattern looks like: in the 2026 seed one per size, `line` (2), `triangle` (3), `quadrilateral` (4), `pentagon` (5), `hexagon` (6), each holding every combination of that many of the 10 checkpoints. A shape has many patterns. Completing any one completes the shape. |
| **Selection** | The shape a team is currently attempting. At most one per team. |
| **Attempt** | Everything between a selection and the next selection, clear or cast. Its scans are the stack. |
| **Stack** | The attempt's accepted scans, oldest at the **bottom**, newest on **top**. It holds **one scan per waypoint**; any member may scan any number of them. Its correct scans always fit inside one pattern of the selected shape, so there are at most **6** (`game.max_pattern_points`) of them, plus at most **one wrong scan**, always on top. Capacity: **7** (`game.max_pattern_points` + 1). Scans never leave on their own: only a selection, clear or cast empties it. |
| **Wrong scan** | A scan whose waypoint, together with the scans already in the stack, isn't part of any pattern of the selected shape that the team can cast now: unlocked, and not of its cooldown class (`!FitsShapeCastable(shape, set, unlocked points, cooldown)`). It's **pushed** (not rejected), marked `wrong`, and the player is **told**: outcome `wrong_scan`, and `stack[].wrong: true` on every phone. |
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
| **Catch-up** | `catchUp`: applies any attack and any QR cooldown end that has fallen due, as its own change. The team's timer calls it, and so does every write before validating, so neither is ever lost to a late timer. |
| **Visited waypoints** | Every waypoint the team has ever had a scan accepted at, under any selection, wrong scans included. |
| **Clock** | Where the store gets time: `Now()` (the time selections, scans and casts are stamped with, and what health and QR cooldowns are derived against) and `AfterFunc(d, f)` (the teams' timers, for attacks and QR cooldown ends). `systemClock` is the real one (`time.Now`, `time.AfterFunc`); tests pass a fake through `Rules.Clock`, whose timers fire only when the test moves its time. |
| **Game rules** | What `GET /api/game/shapes` returns: the selectable shapes (the client's "types") with their pattern counts, counts by size (`patterns_by_points`), sizes, spell classes (`classes`) and damage ranges, every spell class (`spell_classes`), plus `health` (starting health), `unlocks` (the unlock schedule) and `max_pattern_points`. Static while the backend runs. |
| **Leader** | The team's leader (teams' `Team.LeaderID`). **Only the leader selects a shape and clears the selection** (`403 not_leader` for anyone else), so only the leader restarts a poisoned attempt; every member scans. Checked under the entry lock against teams' current roster (`isLeader`): an admin removing the leader names a new one. |
| **Unlock schedule** | `game.unlocks` (`Rules.Unlocks`, `unlocks` in `Game`): bosses defeated → the most waypoints a pattern the team can cast may have from then on. As shipped `0 = 3`, `2 = 4`, `4 = 5`, `5 = 6`: lines and triangles from the start, quadrilaterals after boss 2, pentagons after boss 4, hexagons after boss 5. A pattern bigger than the team's current step is **locked**: it can't be cast (an instant cast is `409 pattern_locked`), a shape made only of locked patterns can't be selected (`409 shape_locked`), and in a shape mixing sizes a scan that could only grow into a locked pattern is a wrong scan. In the 2026 seed every shape is one size, so `quadrilateral`, `pentagon` and `hexagon` are locked at the start and open one by one. (2025's seed had nothing locked.) |
| **Spell cooldown** | `cooldown(State)`, `cooldown_class` in JSON: the spell class the team can't cast now, its last cast's (`class_4` after a 4-point spell), until it casts a spell of another class. Its patterns count as unavailable: a shape made only of them can't be selected (`409 shape_on_cooldown`, listed in `cooldown_shapes`), and a scan that could only grow into one is wrong. Derived from the cast log, so nothing new is stored and a restart rebuilds it; a defeated boss doesn't clear it. |
| **QR cooldown** | `State.QRCooldowns`, `qr_cooldowns` in JSON: the waypoints the team can't scan now. A cast at *T* disables every waypoint of its pattern for the team until *T* + **5 min** (`game.qr_cooldown`); scanning one is refused (`409 waypoint_on_cooldown`), recording nothing. Other waypoints stay open and selecting isn't affected. Ends with nobody acting (`qr_cooldown_ended`). Derived from the cast log and the clock, so nothing new is stored and a restart rebuilds it. See [QR cooldown](#qr-cooldown). |
| **Powerup** | A temporary effect a team holds and its **leader** uses (`State.Powerups`, `powerups` in JSON). Four kinds: `damage_multiplier` (the next cast deals ×**2**, `powerups.damage_multiplier`), `heart` (+**1** health, `powerups.heart`, never above `game.health`), `short_qr_cooldown` (for **5 min**, `powerups.short_qr_window`, casts cool their waypoints for **2 min**, `powerups.short_qr_cooldown`), `instant_cast` (a named pattern is cast at once, with no scans, past the class and QR cooldowns). At most one of each kind held; an admin grants them, or a team earns one by answering a quiz right. Not a spell. See [Powerups](#powerups). |
| **Quiz answer** | A team's one answer to a game-wide quick-time event (a quiz, owned by the [`qte`](../qte/README.md) use case): any member answers, first answer wins (`State.QTEAnswers`; `qte` in JSON is the latest). A right one earns the QTE's powerup. See [Earned by quizzes](#earned-by-quizzes). |
| **Unlocked points** | `unlockedPoints(State)`, `unlocked_points` in JSON: the most waypoints a pattern the team can cast may have: the points of the last unlock-schedule step whose bosses the team has defeated, **3** at the start, **4** after boss 2, **5** after boss 4, **6** after boss 5 (`game.max_pattern_points`). `locked_shapes` lists the shapes whose every pattern is bigger. |
| **Finished** | A team that defeated its last boss. The cast that did it calls `Rules.OnFinished` (teams' `Finish`, wired in `server.Build`) **after releasing the team's lock**; the team becomes `FINISHED`, and from then on every write here is `409 not_in_game` (`inGameTeam`). `Load` finishes a team it finds with every boss defeated but still `IN_GAME`. |
| **Cooldown reset** | An admin's `ResetCooldowns`: `State.CooldownsReset` becomes the cast log's length, and only casts after that position count for the spell and QR cooldowns. **A position, not a time**, so a cast made in the same instant as the reset still starts its cooldowns (the game-loop simulation found that a time-based reset dropped it). Stored in `team_boss.cooldowns_reset_casts` (migration 0013). |
| **Standing** | A team's place for the leaderboard (`Standings()`): bosses defeated, when its latest boss fell, damage on its current boss. |

**Client words.** The client's **"type"** is our **Shape**, its **"shape"** is our **Pattern**, its **"point"** is our **Waypoint**, and its **"buffer"** is our **Stack**. This README and the code always use the code's words. The full vocabulary, spells included, is in [`internal/atlas/README.md`](../atlas/README.md).

## The game flow
0. **Start.** When the leader starts the team (teams' `Start`; any size from 1 to **6** (`teams.size`) may start, once every non-leader is ready), every member's stream gets the `IN_GAME` `team` and then the team's `team_state`, empty: no selection, no stack, no progress, `poisoned: false`, health **10** (`game.health`), and boss 1's attack clock started at the team's start. teams calls `PublishTeam` through its `Rules.OnStart` hook, wired in `server.Build`, because teams may not import teamstate (AGENTS.md §4). The play screen has its state at once, without reconnecting.
1. **Select.** **The leader** selects a shape: `PUT /api/teams/me/state/selection {"shape":"quadrilateral"}` (anyone else: `403 not_leader`). **Every selection purges the stack**, reselecting the same shape included. Reselecting is how a team restarts an attempt. A shape whose patterns are all bigger than the team's unlocked points is refused (`409 shape_locked`) until it has defeated enough bosses (`game.unlocks`: quadrilaterals after boss 2, pentagons after boss 4, hexagons after boss 5). A shape whose unlocked patterns are all of the team's **cooldown class** (its last cast's) is refused too (`409 shape_on_cooldown`) until the team casts a spell of another class.
2. **Scan.** Members scan waypoints: `POST /api/teams/me/state/scans {"qr":"2001:1760000000.…"}`. The token is verified first (signature and freshness, through the `qr` use case); then the waypoint it names is used exactly as the raw ID was. No time limit, and any member may scan any number of the pattern's waypoints (one player can scan a whole pattern). A scan is **rejected with an error and not pushed** if:
   - the token is malformed or tampered: `400 invalid_qr`
   - the token is outside its validity window: `409 qr_expired`
   - no shape is selected: `409 no_shape_selected`
   - the attempt is **poisoned**: `409 selection_poisoned`
   - its **waypoint is already in the stack**, whoever scanned it, the same member included: `409 waypoint_already_scanned`
   - the team cast a spell with that waypoint less than **5 min** ago (`game.qr_cooldown`): `409 waypoint_on_cooldown`

   Otherwise it's pushed on top. If the waypoint, with the stack, can't be part of any pattern of the selected shape **that the team has unlocked**, it's pushed **marked wrong**, the response says `wrong_scan`, and the attempt is poisoned.
3. **Complete (cast).** The moment the stack is **exactly** one of the selected shape's patterns, the pattern and its shape are marked complete, the cast is appended to the cast log (`State.Casts`, `team_cast`) and becomes `last_cast`, its damage (× the team's multiplier) goes to the current **boss** (a defeat moves the team to the next; see [Bosses](#bosses)), its waypoints go on the team's **QR cooldown** for **5 min** (`game.qr_cooldown`), and **the stack and the selection are purged**. The team selects again for its next attempt.
4. **Restart.** After a wrong scan, the leader reselects (the same shape or another): the stack, wrong scan included, is purged and the team scans again.
5. **Give up.** The leader's `DELETE /api/teams/me/state/selection` clears the selection and the stack.
6. **The boss attacks** every **10 min** (`game.boss_attack.interval`) of the phase, for its `game.boss_attack.damage`, with nobody acting: the server lowers health and publishes `boss_attacked` (`knocked_out` when health reaches 0). Defeating the boss starts the next boss's clock. A knocked-out team can't select or scan until an admin revives it. See [Boss attacks and health](#boss-attacks-and-health).
7. **QR codes open again** **5 min** (`game.qr_cooldown`) after the cast that used them, with nobody acting: the server drops them from `qr_cooldowns` and publishes `qr_cooldown_ended`. See [QR cooldown](#qr-cooldown).
8. **Powerups.** The leader uses a held powerup (`POST /api/teams/me/state/powerups/{kind}`): it arms the next cast, heals, starts a short-QR-cooldown window, or casts a pattern at once. A window ends with nobody acting (`powerup_ended`). See [Powerups](#powerups).
9. **Quizzes.** While an admin's quiz is open (the game-wide `qte` event), any member answers for the team (`POST /api/teams/me/state/qte/{qte_id}/answer`): right or wrong at once, the powerup with a right one (`qte_answered`). See [Earned by quizzes](#earned-by-quizzes).

**"Part of a pattern" narrows as the team scans.** The team selects a shape, not a pattern, so the first scan is correct if it's in *any* pattern of the shape. Each later scan must keep the stack inside at least one pattern of the shape that contains everything already scanned. A scan that doesn't is wrong.

**Why the shape is selected first.** With the 2026 content every pair is a line and every three a triangle, so a team scanning a quadrilateral passes through a line after its second scan and a triangle after its third. Only the selected shape can complete, which is what makes bigger shapes possible.

**With the 2026 content a wrong scan can't happen.** Every combination of the 10 checkpoints is a pattern, so any scan of a checkpoint fits the selected shape while the stack is smaller than its size. What limits play instead: the **unlock schedule** (quadrilaterals need 2 bosses defeated, pentagons 4, hexagons 5: `shape_locked`), the **spell cooldown** (every shape is one class, so the shape last cast is `shape_on_cooldown`), and the **QR cooldown**. The wrong-scan rules below still hold for content with a waypoint in no pattern of the shape; the tests add one, `2099` (`Nowhere`), to exercise them.

**Why a wrong scan is pushed, not rejected.** The scan really happened: someone walked to that waypoint. Recording it (in the stack, in `team_scan`, in visited `waypoints`) keeps the audit trail honest, and showing it marked on every phone tells the whole team *which* point broke the attempt and who scanned it.

**Why a wrong scan poisons instead of being dropped.** If the server silently dropped the wrong scan, a team could probe which waypoints belong to the selected shape for free, one scan at a time, with no cost. Poisoning makes a mistake cost a restart: every correct scan of the attempt is lost too.

**Why no time limit.** The game used to give a pattern's scans 1 minute (plus grace) bottom to top. That's gone: an attempt lasts until it casts, is restarted or is cleared, so there are no stale scans. The pressure of time comes from the bosses instead: a slow team loses health, not scans.

**Why only the leader selects.** A selection purges the stack, so any member selecting could wipe teammates' scans mid-attempt, by accident or in a race. One person choosing the type (and deciding when to restart or give up) makes the team's plan unambiguous; scanning stays open to everyone.

**Why bigger patterns are locked, and why as "they don't exist yet".** Bigger patterns are harder and deal more damage, so each size is a reward for getting through the bosses before it: bosses 1 and 2 are fought with lines and triangles, boss 3 with quadrilaterals, boss 5 with pentagons, Gideon with hexagons. Treating a locked pattern as absent (the fit check counts only unlocked patterns) keeps one rule for everything: a scan that can only lead to a locked pattern is simply wrong, so a team can't half-build one and wait.

**Why a cooling QR code is refused, not pushed as wrong.** The team can't help that the waypoint is cooling, and it's in `qr_cooldowns` for everyone to see: refusing it (nothing recorded, no event) lets the team wait or walk elsewhere without losing its attempt. A wrong scan is a mistake about the shape, so it costs a restart; a cooling scan is just early.

**Retries.** A phone resending a scan gets `409 waypoint_already_scanned` (or `selection_poisoned`, if its first copy was wrong); the app should treat that as "already in" and refresh from the returned state or the live event.

## How each file works

### `api.go`: routes
- **`Routes`** registers eight routes behind the auth middleware (see [API](#api)). **No admin routes here:** revive and cooldown reset are the `admin` use case's routes ([`internal/admin/README.md`](../admin/README.md)), which call `Revive`, `ResetCooldowns` and `TeamView`.
- **Handlers:**
  - `shapes` returns `Store.Game()` to any signed-in user, in a team or not.
  - `getMine` returns the caller's team state (`Store.ForUser`).
  - `casts` returns the caller's team's cast log, newest first (`Store.CastsForUser`).
  - `selectShape` decodes `{"shape"}` and calls `Store.Select` (leader only).
  - `clearSelection` calls `Store.ClearSelection` (leader only).
  - `scan` decodes `{"qr"}` and calls `Store.Scan`, returning a `ScanResult`.
  - `usePowerup` resolves `{kind}` (`PowerupByID`, else `404 unknown_powerup`) and calls `Store.Activate`, or, for `instant_cast`, decodes `{"pattern": [IDs]}` (empty or a repeated ID: `400 invalid_body`) and calls `Store.InstantCast`.
  - `answerQTE` parses `{qte_id}` (else `400 invalid_qte_id`), decodes `{"option": n}` (missing or negative: `400 invalid_body`) and calls `Store.AnswerQTE`.
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
  - `QRCooldown`: one waypoint on the team's QR cooldown (`Waypoint`, its atlas index; `Until`, when it opens). `State.qrCooldownUntil(w, now)` returns `w`'s `Until` and true while `now` is before it.
  - `Store.qrCooldowns(casts, reset, now)`: the active QR cooldowns, derived from the cast log: walking it newest first, down to index `reset` (`State.CooldownsReset`: casts before an admin's reset don't count), each cast's waypoints not already seen, until `cast.At + Rules.QRCooldown`, stopping at the first cast whose cooldown has ended at `now` (every older one has too); sorted by `Until`, then waypoint index. `nil` when `Rules.QRCooldown` is 0. Used by `stateOf`, `Scan` (on a cast) and `catchUp` (on an expiry).
  - `BossProgress.nextAttack(bosses)`: `PhaseStart + (Attacks + 1) × interval`; `ok` false if every boss is defeated or the team is knocked out.
  - `deal(progress, damage, now, bosses)`: applies one cast: returns the new progress, the damage dealt (`math.Round(damage × Multiplier)`), the serial of the boss hit, and whether it was defeated. If every boss is already defeated it returns the progress unchanged, 0, 0, false. On a defeat it increments `Defeated`, resets `Damage` to 0 (the rest of the cast is lost), and **starts the next boss's phase**: `PhaseStart` = `now`, `PhaseHealth` = the current `Health`, `Attacks` = 0. The progress passed in must already be at `now` (`Scan` caught up first). Called by `Scan`.
  - `View`, `SelectionView`, `CastView`, `BossView`, `StackScanView`: the JSON shapes. `CastView` is `last_cast` and each entry of the cast log: `pattern` (sorted waypoint IDs), `shape`, `class` (the pattern's spell class ID, e.g. `class_4`), `cast_by`, `cast_at`, `already_completed`, `damage`, `dealt`, `boss` (serial, or `null`), `defeated_boss`. `StackScanView` carries `wrong`; `View` carries `poisoned`, `bosses_defeated`, `boss` (`BossView{serial, damage, phase_started_at, attacks, next_attack_at}`, or `null`), `damage_multiplier`, `health`, `knocked_out`, `unlocked_points`, `locked_shapes`, and `qr_cooldowns` (`QRCooldownView{waypoint_id, until}`).
  - `Game`, `GameShape`, `DamageRange`: the JSON of `GET /api/game/shapes`, including each shape's `damage` range (`{"min": …, "max": …}`), `patterns_by_points` (its patterns counted by size) and `classes` (the spell class of each size, in `points` order), the game's `spell_classes` (every class, ascending by points), the game's `health` (`game.health`) and `unlocks` (`[]Unlock`, the schedule).
  - `Unlock`: one step of the schedule, `{"after_bosses": …, "points": …}` (`AfterBosses`, `Points`). `unlockSchedule(Rules.Unlocks, MaxPatternPoints)` sorts the map into steps by `AfterBosses`; an empty map is one step `{0, MaxPatternPoints}` (nothing locked).
  - `Store.unlockedPoints(State)`: the `Points` of the last step of `Game.Unlocks` whose `AfterBosses` ≤ `State.Boss.Defeated` (0 if none). Used by `view`, `Select`, `Scan` and `InstantCast`.
  - `Store.shapeLocked(shape, unlocked)`: true if the shape's smallest pattern (`Game.Shapes[shape].Points[0]`, the sizes being ascending) is bigger than `unlocked`. A shape with no patterns isn't locked.
  - `Store.cooldown(State)`: the spell class of `State.LastCast()`'s pattern (`Atlas.Pattern(p).Spell.Class`), or `""` before the first cast, or when no cast came after an admin's reset (`len(Casts) <= CooldownsReset`).
  - `Store.shapeOnCooldown(shape, unlocked, cooldown)`: true if the shape has sizes of at most `unlocked` and every one's class (`Game.Shapes[shape].Classes`, aligned with `Points`) is `cooldown`. False when `cooldown` is `""` or the shape is locked.
- **`Stack` methods** (all on values; none modifies the receiver):
  - `NewStack(maxPoints)` is an empty stack with `Cap` = `maxPoints` + 1 (exported so tests can build one).
  - `Len()` counts scans.
  - `At(i)` is the i-th from the bottom (`At(0)` the oldest).
  - `Push(sc)` returns the stack with `sc` on top. It **panics** on a full stack: `Scan` never pushes onto one (correct scans fit one pattern, and nothing is pushed after a wrong scan), so a full push would be a bug.
- **`newGame(atlas, rules)`** builds the `Game` once, in `Load`: for each shape in index order (`ShapeCount`, `Shape(i)`), its ID and name, how many patterns it has (`len(ShapePatterns(i))`), the distinct sizes of those patterns (`Pattern(p).Waypoints.Len()`), ascending, and the minimum and maximum `Pattern(p).Spell.Damage`, and the count of its patterns of each size (`PatternsByPoints`); plus `MaxPatternPoints`, `Health` and `Unlocks` (through `unlockSchedule`) from `Rules`.
- **Reads**, all from memory:
  - `Game()`: the prebuilt `Game`; no lock, it never changes
  - `ForTeam(teamID)` → `State` (the empty state for a team with no entry). Reads return the snapshot as last applied: health is as of the last catch-up (the timer keeps it current), not recomputed on read
  - `ForUser(userID)` → `View`, or `ErrNotInTeam`
  - `CastsForUser(userID)` → `[]CastView`, newest first, or `ErrNotInTeam`; `[]` before the first cast
  - `Snapshot(userID)`: the live event for a new stream, of type `snapshot`
  - `TeamView(teamID)` → `View`: a team's state by ID, for the admin use case's teams list
  - `Standings()` → `[]Standing{TeamID, BossesDefeated, LastDefeatAt, Damage}`: every team with an entry, for the leaderboard; `LastDefeatAt` is the latest `DefeatedBoss` cast's time (zero if none). Copies the index under its read lock, each snapshot lock-free
- **`view`** converts indexes and bits to IDs through the atlas (`WaypointIDs`, `Pattern`, `Shape`, `Waypoint`), times to UTC, builds `last_cast` from `State.LastCast()` (`null` when the log is empty), lists every stack scan with its `wrong` flag, sets `poisoned` from `State.Poisoned()`, and sets the boss fields: `bosses_defeated`, `damage_multiplier`, `health` and `knocked_out` from `State.Boss`, `unlocked_points` from `unlockedPoints` and `locked_shapes` (every shape `shapeLocked` at that size, in index order; `[]` when none), `cooldown_class` from `cooldown` (`null` when `""`) and `cooldown_shapes` (every shape not locked but `shapeOnCooldown`, in index order), `qr_cooldowns` from `State.QRCooldowns` in its order (`[]` when none), and `boss` from `Bosses.At(Defeated).Serial` with `Damage`, `PhaseStart`, `Attacks` and `next_attack_at` (`nextAttack`, `null` when knocked out), or `null` when `Defeated` ≥ `Bosses.Count()`.
- **`castView`** turns one `Cast` into a `CastView`, the shape being the pattern's `Shape`, `class` its `Spell.Class` and `damage` its precomputed `Spell.Damage` (both from `pattern`, loaded by the atlas), and `dealt`, `boss` (`null` when the cast's `Boss` is 0) and `defeated_boss` from the `Cast`. Used by `view` and `CastsForUser`.

- **Powerups:** `Powerup` (the four kinds, `ID()`, `PowerupByID`), `PowerupSet` (a bit per kind), `Powerups{Held, MultiplierArmed, ShortQRUntil}` inside `State`, and `powerupsView` → `PowerupsView{held, active}` (both in kind order, never null). `Cast` carries `Instant`, `Boosted` and `ShortQR`. **`qrCooldowns`** gives each cast its own duration (`ShortQRCooldown` when `Cast.ShortQR`, else `QRCooldown`), walks back until a cast older than the longer one, and cools each waypoint until its **latest** end.

### `write.go`: writes, each written through
- **`Select`**, **`ClearSelection`**, **`Scan`**, **`Revive`**, **`ResetCooldowns`**, **`GrantPowerup`**, **`Activate`**, **`InstantCast`** and **`AnswerQTE`** each follow the [write-through](#the-write-through) steps, numbered in the code.
- **Catch-up first.** Right after `1. lock` and `isMember`, `Select`, `ClearSelection` and `Scan` call **`catchUp`**, which applies any boss attack or QR cooldown end that has fallen due (its own change and its own event) and returns the current state; the write validates against that. Then `Select` and `ClearSelection` refuse anyone but the leader (`isLeader`, `ErrNotLeader`, `403 not_leader`); `Select` and `Scan` refuse a knocked-out team (`ErrKnockedOut`, `409 knocked_out`); `ClearSelection` doesn't (giving up is harmless); and `Select` refuses a shape whose patterns are all locked (`shapeLocked`, `ErrShapeLocked`, `409 shape_locked`), then one whose unlocked patterns are all of the cooldown class (`shapeOnCooldown`, `ErrShapeOnCooldown`, `409 shape_on_cooldown`). `Scan`'s wrong-scan check is `Atlas.FitsShapeCastable(shape, stack ∪ {w}, unlockedPoints, cooldown)`. `Revive` catches up too, so the event order is the attack, then the revive.
- **`catchUp(e, teamID)`**: `cur.Boss.at(now)`, whether the soonest QR cooldown (`QRCooldowns[0]`) has ended (`now` ≥ its `Until`), and whether a short_qr_cooldown window has (`now` ≥ `ShortQRUntil`, then cleared). One event: `knocked_out` > `boss_attacked` > `powerup_ended` > `qr_cooldown_ended`. If neither changed, returns `cur`. Otherwise it builds the next state with the new progress, the QR cooldowns re-derived (`qrCooldowns(Casts, CooldownsReset, now)`) if one ended, and `Version.Next`, **skips persist** (memory-only, AGENTS.md §6: health is derived from the stored phase and the clock, QR cooldowns from the cast log and the clock, and `Load` reproduces both exactly), applies, and publishes **one** event: **`boss_attacked`** (or **`knocked_out`** if health is now 0) if an attack fell due, else **`qr_cooldown_ended`**. Several due attacks and expiries become one change.
- **`due(teamID)`**: the team timer's callback, run by the `Clock` in its own goroutine. `1. lock` of the **existing** entry only (looked up in the index, never created: a team `TeamDeleted` removed isn't brought back), then nothing if the entry is closed (`entry.closed`: `Close` or `TeamDeleted`), else `catchUp` and `schedule` for whatever is next. A timer that fires early finds nothing due and re-arms; one that fires late applies everything due.
- **`Revive(ctx, teamID)`**: an admin's revive (called by the admin use case). `2. validate, before 1. lock`: `Teams.ByID` (unknown → `ErrTeamNotFound`, `404 team_not_found`), `IN_GAME` (else `ErrNotInGame`, `409 not_in_game`). `1. lock`, `catchUp`, `3. build`: a new phase of the same boss, `PhaseStart` = now, `PhaseHealth` = `Health` = `Rules.Health`, `Attacks` = 0. `4. persist`: `upsertBossPhase`. `5. apply`, `6. publish` **`revived`**, and `schedule`. Allowed whether or not the team is knocked out, so it also heals; `Defeated`, `Damage` and the multiplier are kept.
- **`ResetCooldowns(ctx, teamID)`**: an admin's cooldown reset (called by the admin use case). `2. validate, before 1. lock`: `Teams.ByID` (`ErrTeamNotFound`), `IN_GAME` (`ErrNotInGame`). `1. lock`, `catchUp`, `3. build`: `CooldownsReset` = `len(Casts)`, `QRCooldowns` = none, `Version.Next`. `4. persist`: `upsertCooldownsReset`. `5. apply`, `6. publish` **`cooldowns_reset`**, and `schedule` (a pending QR expiry is no longer due).
- **`applyCast(cur, next, p, by, now, instant)`**: the cast itself, shared by `Scan`'s completion and `InstantCast`: the pattern and shape completed, the selection and stack purged, `deal` (× `PowerupMultiplier` when `MultiplierArmed`, which it clears: the log grows past the activation length), `Cast.ShortQR` when a window runs, the log appended copy-on-write, QR cooldowns re-derived. Returns the cast, `spell_cast` or `boss_defeated`, and whether the last boss fell.
- **`GrantPowerup(ctx, teamID, kind)`**: `2. validate, before 1. lock`: kind, `Teams.ByID` (`ErrTeamNotFound`), `IN_GAME` (`ErrNotInGame`). `1. lock`, `catchUp`. Already held: `GrantResult{Granted: false}`, nothing written or published. Else `Held.With(kind)`, `upsertPowerupHeld`, `powerup_granted`. A knocked-out team can be granted one.
- **`Activate(ctx, userID, kind)`**: kinds 1–3 (an `instant_cast` is `InstantCast`'s). After `1. lock`, `isMember`, `catchUp`: leader (`ErrNotLeader`), knocked out (`ErrKnockedOut`), held (`ErrPowerupNotHeld`), still running (`ErrPowerupActive`: an armed multiplier, a window not ended), a heart at full health (`ErrFullHealth`). Build: `Held.Without(kind)`, then the effect: `MultiplierArmed` (its `activated_casts` = `len(Casts)`), `PhaseHealth` and `Health` + `min(PowerupHeart, Health − health)` (the phase start unchanged), or `ShortQRUntil = now + ShortQRWindow`. Persist `updatePowerupActivated` (a heart: `useHeart`), publish `powerup_activated`, `schedule`.
- **`InstantCast(ctx, userID, waypointIDs)`**: before the lock, `Atlas.SetOf` (`ErrUnknownWaypoint`) and `Atlas.PatternFor` (`ErrUnknownPattern`); under it, leader, knocked out, held, then the pattern's size against `unlockedPoints` (`ErrPatternLocked`). **No class or QR cooldown check.** Build: `Held.Without(instant_cast)`, `applyCast(…, instant)`; persist `insertInstantCast`; publish the cast's type; the finish hook as `Scan`'s; `schedule`. Visited waypoints don't change: nothing was scanned.
- **The finish hook.** `Scan` (and `InstantCast`) registers, before `1. lock`, a deferred call that runs **after** the deferred unlock: if the cast defeated the last boss, it calls `Rules.OnFinished(teamID)`. Whether it did is decided once the write has settled, never from the build: after `5. apply`, `applyCast`'s answer; after a failed persist, `finishedBy`, whether `reconcile`'s reload has every boss defeated where the state before didn't (a write that timed out may have committed). A write that failed and didn't commit finishes nothing. Outside the lock because teams' writes call back into team state (`RosterChanged`, `TeamDeleted`) while holding teams' lock: holding teamstate's lock while waiting for teams' would be the other order, a deadlock.
- **`TeamDeleted(teamID)`** is teams' `Rules.OnDelete` (wired in `server.Build`): an admin deleted the team, and Postgres has removed its rows (cascade). It removes the entry from the index (index lock only), then, under the entry's own lock, stops its timer and marks it `closed`, **never holding both** (`Close` takes them the other way round). **Memory-only** (AGENTS.md §6): the data is gone from Postgres, which is what `Load` would rebuild. Publishes nothing (teams sent `no_team`).
- **Lock-free checks first** (§6 variation (a)): `Scan` verifies the token (`QR.Verify`, mapping its errors to `ErrInvalidQR` / `ErrExpiredQR`) and resolves the waypoint with `Atlas.WaypointIndex`, `Select` the shape with `Atlas.ShapeIndex`, all before the team and the lock.
- **`Scan`'s checks**, in order, under the lock: no selection → `ErrNoShapeSelected`; `cur.Poisoned()` → `ErrSelectionPoisoned`; the waypoint already in the stack → `ErrWaypointAlreadyScanned`; the waypoint on the team's QR cooldown (`cur.qrCooldownUntil(w, now)`) → `ErrWaypointOnCooldown`. Then `Atlas.FitsShapeCastable(shape, stack ∪ {w}, unlockedPoints(cur), cooldown(cur))` decides whether the scan is **wrong** (pushed, marked `Wrong`, outcome `wrong_scan`): only patterns the team can cast now count (unlocked, and not of its cooldown class), so a scan that could only grow into a locked or cooled-down one is wrong too. Completion is `!wrong` and `Atlas.PatternFor(stack ∪ {w})` (one map lookup) being a pattern whose `Atlas.Pattern(p).Shape` is the selected shape. On completion, `Atlas.WaypointIDs` gives `ScanResult.Pattern` and **`applyCast`** builds the cast: `Atlas.Pattern(p).ID` is the key written to `team_pattern` and `team_cast`, `AlreadyCompleted` is `cur.Patterns.Has(p)`, the cast's damage goes to the boss (`deal(cur.Boss, Pattern(p).Spell.Damage, factor, bosses)` sets `next.Boss` and the cast's `Dealt`, `Boss`, `DefeatedBoss`; a defeat makes the event type `boss_defeated` instead of `spell_cast`, and starts the next boss's phase), the new `Cast` is appended to the log, its waypoints' QR cooldowns are re-derived from it (`next.QRCooldowns = qrCooldowns(next.Casts, next.CooldownsReset, now)`: each until `now` + `game.qr_cooldown`), and after publishing `Scan` calls `schedule`. The log is appended **copy-on-write**: `append(slices.Clip(cur.Casts), cast)`. `Clip` sets the slice's capacity to its length, so `append` always allocates a new backing array and the current snapshot's slice is never written to.
- **`inGameTeam`** fetches the caller's team and requires `IN_GAME` (lock-free: a started team's status never goes back).
- **`isMember(teamID, userID)`** reads the team's current roster through `Teams.ByID` (lock-free) and reports whether the user is in it. `Select`, `ClearSelection` and `Scan` call it **again under the entry lock**, right after `1. lock`, and refuse with `ErrNotInTeam` (`404 not_in_team`) if an admin removed the caller since `inGameTeam` looked them up. A started team's roster can shrink (teams' `RemoveMember`), so the lock-free lookup alone isn't enough.
- **`isLeader(teamID, userID)`** reads the team through `Teams.ByID` (lock-free) and reports whether the user is its leader **now**. `Select` and `ClearSelection` call it under the entry lock, after `catchUp`: an admin removing the leader names a new one (teams' `RemoveMember`), and from then on only the new leader passes.
- **`RosterChanged(teamID)`** is teams' `Rules.OnRosterChange` (wired in `server.Build`): an admin removed a member from a started team. It locks the entry (`lockTeam`) and drops `entry.members`, the roster cache, so the next publish re-reads the roster from teams and no longer reaches the removed player. **Memory-only** (AGENTS.md §6): the roster is teams' data, already saved. **Publishes nothing:** the team's `State` didn't change (the removed player's scans and casts stay), and teams has already sent the rest `team` and the removed player `no_team`.
- **`PublishTeam(teamID)`** sends the team's current state to every member as `team_state`, changing nothing, and arms the team's first timer. teams calls it through `teams.Rules.OnStart` (wired in `server.Build`) right after a team starts, so members get the new team's empty state after its `team` event. Steps: `2. validate, before 1. lock` (`Teams.ByID`: the team exists and is `IN_GAME`, else nothing is published), `1. lock` (`lockTeam`, which creates the empty entry if the team had none, its phase starting at the team's `started_at`), `6. publish` (`publish`, which fills the entry's roster cache on this first publish of the started team), then `schedule`. Not a write: no build, persist or apply. It takes the entry lock so its event is ordered with the team's writes.
- **`AnswerQTE(ctx, userID, qteID, option)`**: `2. validate, before 1. lock`: `QTEs.ByID` (none, or a draft: `ErrUnknownQTE`), the option in range (`ErrInvalidOption`), the team in a game. `1. lock`, `isMember`, `catchUp`. Then already answered (`ErrQTEAlreadyAnswered`), the QTE read again (an admin may have ended it) and `now` ≥ its `ends_at` (`ErrQTEClosed`), knocked out (`ErrKnockedOut`). Build: `correct`, `granted` (correct and the kind not held), the answer appended copy-on-write, the kind added to `Held` if granted. Persist `insertQTEAnswer` (its refusal after the stored `ends_at` is `ErrQTEClosed`, with no reload: nothing was applied), publish `qte_answered`. **`QTEStats()`** counts every team's answers per QTE (answered, correct, granted, per option) for the admin overview.
- **Declared here:** the errors (`ErrInvalidOption`, `ErrQTEAlreadyAnswered`, `ErrUnknownQTE` and `ErrQTEClosed` (qte's own), `ErrUnknownPowerup`, `ErrPowerupNotHeld`, `ErrPowerupActive`, `ErrFullHealth`, `ErrUnknownPattern`, `ErrPatternLocked`, `ErrNotInGame`, `ErrUnknownWaypoint`, `ErrUnknownShape`, `ErrNoShapeSelected`, `ErrWaypointAlreadyScanned`, `ErrKnockedOut`, `ErrNotLeader` (teams' own), `ErrShapeLocked`, `ErrShapeOnCooldown`, `ErrWaypointOnCooldown`, `ErrTeamNotFound` (teams' own, for `Revive`), `ErrSelectionPoisoned`, `ErrInvalidQR`, `ErrExpiredQR`), the `Outcome…` constants (`scan_added`, `wrong_scan`, `pattern_completed`), and `ScanResult`.

### `db.go`: SQL
Only the per-team tables. The content tables are read by the atlas (`internal/atlas/db.go`).
- **`selectTeams(ctx, teamID, stackSize)`** reads every team's stored state, or one team's (`teamID != ""`), into `teamRows`: distinct visited waypoints, completed patterns, **every** cast (`casts`, `castRow`s with `dealt`, `COALESCE(boss_serial, 0)` and `defeated_boss`, `ORDER BY team_id, id`: oldest first), the `team_boss` row if any (`boss`, a `bossRow`: `defeated`, `damage`, `multiplier`, the nullable `phase_health`, `phase_started_at`, and `cooldowns_reset_casts`), selection, and the stack. Used by `Load` and `reconcile`, with `stackSize` = `game.max_pattern_points` + 1. The stack query is a `JOIN LATERAL`: for each selection, the team's newest `stackSize` `team_scan` rows with `id > after_scan_id`, returned oldest first, with the `wrong` flag.
- **`insertScan`**: one `team_scan` row, with the `wrong` flag (a scan that doesn't complete anything: a correct one, or a wrong one).
- **`insertCompletingScan(…, cast, progress)`**: one `pgx.BeginFunc` transaction: `INSERT team_scan` (the scan itself; `wrong` is always false here), then `insertCastRows`.
- **`insertCastRows(ctx, tx, …)`**: a cast's rows, inside the caller's transaction, in order (`team_cast` with `instant`, `boosted`, `short_qr_cooldown`):
  1. `INSERT team_pattern … ON CONFLICT DO NOTHING`: the first completion only
  2. `INSERT team_cast (team_id, pattern_id, cast_by, cast_at, already_completed, dealt, boss_serial, defeated_boss)`: every cast, repeats included; `boss_serial` is `NULL` when the cast hit no boss
  3. `upsertBoss`: `INSERT team_boss (team_id, defeated, damage, phase_health, phase_started_at) … ON CONFLICT (team_id) DO UPDATE SET` those four: the progress after the cast, and its phase (a defeat moves it to the next boss). A new row takes the default multiplier (1); an existing row keeps its multiplier, which casts never change
  4. `DELETE team_selection`: which also empties the stack
- **`upsertSelection`**: upserts `team_selection` with `after_scan_id` = the team's latest `team_scan.id` (0 if none). That's the purge: earlier scans (a wrong one included) are no longer after it.
- **`deleteSelection`**: deletes `team_selection`, and fails unless exactly one row went (memory had a selection, so Postgres must too).
- **`upsertBossPhase`**: `Revive`'s write: one `upsertBoss` on the pool (the new phase, at full health).
- **`upsertCooldownsReset(ctx, teamID, casts)`**: `ResetCooldowns`' write: `INSERT team_boss (team_id, cooldowns_reset_casts) … ON CONFLICT (team_id) DO UPDATE SET cooldowns_reset_casts` (a new row takes every other column's default: no progress).
- **`upsertPowerupHeld`**: `GrantPowerup`'s write: `holdPowerup`, `INSERT team_powerup … ON CONFLICT (team_id, kind) DO UPDATE SET held = true WHERE NOT held`, exactly one row (`execOne`).
- **`insertQTEAnswer(ctx, teamID, a, grant)`**: `AnswerQTE`'s write, one transaction: `INSERT team_qte_answer … SELECT … FROM qte WHERE id = $2 AND started_at IS NOT NULL AND option < cardinality(options) AND answered_at < ends_at`; no row can only mean the stored `ends_at` has passed (memory checked the rest, which never changes back), returned as `errAnswerAfterEnd`; the primary key refuses a second answer, then `holdPowerup` when `grant` names a kind.
- **`updatePowerupActivated`** / **`activatePowerup`**: `UPDATE team_powerup SET held = false, activated_at, activated_casts WHERE … AND held`, exactly one row. **`useHeart`**: one transaction, that UPDATE then `upsertBoss`. **`insertInstantCast`**: one transaction, that UPDATE then `insertCastRows` (no `team_scan` row).
- **`upsertBoss(ctx, q, teamID, progress)`**: the one `team_boss` upsert, on the pool or inside a transaction: `defeated`, `damage`, `phase_health`, `phase_started_at`. The multiplier is never written here.
- **Attacks and QR cooldowns write nothing.** `catchUp` and the timer have no `db.go` call: health is derived from the stored phase, QR cooldowns from `team_cast`.

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
  | `FitsShapeCastable(shape, set, maxPoints, cooldown)` | `Scan`: is the scan **wrong**? (`unlockedPoints`, `cooldown`) |
  | `WaypointIDs(set)` | `view` (visited waypoints, completed patterns), `castView` (`pattern`), `ScanResult.Pattern` |
  | `ShapePatterns(shape)` | `newGame`: each shape's pattern count, sizes and damage range |
  | `ShapeCount()` | `newGame`: how many shapes |
  | `Waypoint(i)` | `view`: a stack scan's waypoint ID |
  | `Shape(i)` | `view` (completed shapes, selection), `castView` (`shape`), `newGame` |
  | `Pattern(i)` | `Scan` (the completed pattern's shape and Postgres `ID`), `stateOf` (a completed pattern's shape), `view` (patterns), `castView` (waypoints, shape, `Spell.Damage`), `newGame` |
  | `SpellClasses()` | `newGame`: `Game.SpellClasses` for `GET /api/game/shapes` |
- **`Bosses`** is what team state needs from the bosses use case, satisfied by `*bosses.Bosses`: `Count()` (how many) and `At(i)` (the boss at index `i`, serial `i+1`). Static, so lock-free. Used by `deal` (the current boss's HP and serial), `at` and `nextAttack` (its `AttackInterval()` and `AttackDamage`), `view` (`boss.serial`) and `stateOf` (the load check).
- **`Clock`** is the store's time source, an interface with `Now()` (UTC, truncated to microseconds) and `AfterFunc(d, f) (stop func() bool)` (call `f` in its own goroutine after `d`; `stop` cancels it). **`systemClock`** is the real one: `time.Now().UTC().Truncate(time.Microsecond)` and `time.AfterFunc(d, f).Stop`.
- **`QR`** is what team state needs from the qr use case, declared here (AGENTS.md §4) and satisfied by `*qr.Verifier`: one method, `Verify(token) → (waypoint ID, error)`. It holds no state (the token is derived from the secret and the clock), so `Scan` calls it lock-free. It doesn't check the waypoint exists: `Scan` does that through `Atlas.WaypointIndex`.
- **`QTEs`** is what team state needs from the qte use case, satisfied by `*qte.Store`: `ByID(id)`, lock-free. `AnswerQTE` reads the QTE's start, end, options, right option and powerup through it. qte never imports teamstate (it gets the powerup kinds from `PowerupIDs`, through `server.Build`).
- **`Rules`** are the settings from `config.toml`: `MaxPatternPoints` (`game.max_pattern_points`; the stack holds that many plus one wrong scan), `Health` (`game.health`: starting health and what a revive restores), `Unlocks` (`game.unlocks` through `UnlockSchedule()`: bosses defeated → most waypoints castable; empty locks nothing), `PowerupMultiplier`, `PowerupHeart`, `ShortQRCooldown`, `ShortQRWindow` (`[powerups]`), `QRCooldown` (`game.qr_cooldown`: how long a cast's waypoints stay disabled for the team; 0 turns it off), `WriteTimeout` (`storage.write_timeout`), and, not in config.toml, `Clock` and `Epoch` (nil `Clock` means `systemClock`; tests pass a fake) and `OnFinished func(teamID)` (called once a cast has defeated a team's last boss, without the lock; `server.Build` wires it to teams' `Finish`; nil means no hook).
- **`Load(ctx, pg, atlas, bosses, teams, qr, qtes, hub, log, rules)`** reads every team's stored state (`selectTeams` with `stackSize` = `MaxPatternPoints` + 1), checking it against the atlas and bosses, and builds the `Game` (`newGame`). Then it creates an entry for every `IN_GAME` team in `Teams.All()` that had nothing stored (`lockTeam`), so every started team is attacked, and only once the map is complete arms each entry's attack timer (`schedule`). Every state is brought up to now as it's built (`emptyState` and `stateOf` call `at`), so attacks that fell due while the backend was down are applied at load (and published to nobody: no stream is open yet; clients get them in their `snapshot`). Last, if `Rules.OnFinished` is set and there are bosses, it calls it for every team still `IN_GAME` whose state has every boss defeated (the backend stopped between such a cast and the team being marked `FINISHED`). It runs after `atlas.Load`, `bosses.Load` and `qte.Load` (it's handed them) and after `teams.Load`.
- **`Close()`** stops every pending timer and marks every entry `closed`, so no timer re-arms and one already running does nothing. `main.go` registers it with `srv.RegisterOnShutdown` (after `Hub.Close`); tests call it before loading another store on the same database.
- **`emptyState(teamID)`** is a `State` with an empty stack of capacity `MaxPatternPoints` + 1 and `Boss: {Defeated: 0, Damage: 0, Multiplier: 1, PhaseStart: the team's started_at (from Teams.ByID; now if it has none), PhaseHealth: Rules.Health}`, then brought up to now (`at`): a team without a `team_boss` row is at boss 1 with multiplier 1, attacked since its start.
- **`stateOf(teamID, teamRows)`** rebuilds a `State` from IDs through the atlas, failing on unknown waypoints, patterns or shapes. Every `team_cast` row becomes a `Cast` in `State.Casts`, oldest first (`cast: unknown pattern N` if the atlas lacks it). Stack rows are pushed oldest first, with their `wrong` flag. A `team_boss` row becomes `State.Boss` (`defeated`, `damage`, `multiplier`, and `phase_health` / `phase_started_at` where not `NULL`, else `emptyState`'s full health and team start), then `at(now)`, and `cooldowns_reset_casts` becomes `CooldownsReset`; `State.QRCooldowns` is `qrCooldowns(Casts, CooldownsReset, now)`, so cooldowns that ended while the backend was down are gone; if its `defeated` is more than `Bosses.Count()` (game.bosses lowered mid-event) the load fails (`team_boss: N bosses defeated, but game.bosses is M`).
- **`schedule(e, teamID, st)`** (caller holds `e.mu`) stops the entry's pending timer (`stopTimer`) and, unless the entry is `closed`, arms **one** `Clock.AfterFunc(next − now, due)` for the earliest of `nextAttack` (not ok when knocked out or every boss is defeated) and the soonest QR cooldown's `Until`; nothing if neither exists. Called after every apply that can move either: a cast (`Scan` on completion: new cooldowns, maybe a new phase), the timer itself, `Revive`, `reconcile`, `PublishTeam` and `Load`. A timer isn't re-armed after an ordinary scan or selection: catching up never moves the schedule forward, and a stale timer is harmless. So a knocked-out team still gets its `qr_cooldown_ended`.
- **`lockTeam`** gets or creates a team's entry (with `emptyState(teamID)`) and locks it.
- **`persist`** runs a `db.go` write detached from the request, capped at **5 s** (`storage.write_timeout`).
- **`reconcile`** reloads one team's state from Postgres after a failed write (brought up to now, like `Load`), sets its `Version` to the in-memory one's `Next` (a reload never goes back), publishes it as type `resync`, and re-arms the attack timer (`schedule`). If the reload fails too, memory stays as it was.
- **`publish(e, teamID, st, typ)`**, called with `e.mu` held, sends `team_state` of type `typ` to every member. The recipients are **`e.members`**, the team's roster cached on the first publish that finds the team `IN_GAME` (its roster then only changes by an admin's removal, and `RosterChanged` drops the cache when it does). Until then, and after a drop, it reads the roster through `Teams.ByID`. So a publish reads nothing outside the entry: no index lock, no lookup.
- **`event(teamID, st, typ)`** encodes the envelope once, `live.NewEvent("team_state", typ, view)`; every member's stream gets the same bytes.
- **The type constants** (`GameStarted`, `ShapeSelected`, `SelectionCleared`, `ScanAdded`, `WrongScan`, `SpellCast`, `BossDefeated`, `BossAttacked`, `KnockedOut`, `QRCooldownEnded`, `CooldownsReset`, `Revived`) are declared beside `event`; `live.Snapshot` and `live.Resync` come from `platform/live`. `Scan` picks its type from its outcome: `scan_added` → `ScanAdded`, `wrong_scan` → `WrongScan`, `pattern_completed` → `SpellCast`, or `BossDefeated` when that cast defeated the boss.
- **`Rules.Epoch`** is the process's epoch (from `server.Build`). `emptyState` starts a team at `{epoch, 0}`; `Select`, `ClearSelection`, `Scan`, `Revive`, `ResetCooldowns` and `catchUp` set `next.Version = cur.Version.Next(epoch)` in their build step. `PublishTeam` resends the current version.
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
  clock    Clock                     Rules.Clock, or systemClock: Now, and AfterFunc for team timers

entry
  mu          sync.Mutex             writers to this team queue here
  cur         atomic.Pointer[State]  the current snapshot; readers load it lock-free
  members     []uuid.UUID            the user IDs events go to: the roster, cached
                                     by publish once the team is IN_GAME, dropped
                                     by RosterChanged on an admin's removal; nil
                                     until the next publish refills it. Guarded by mu
  stopTimer   func() bool            cancels the pending timer (next attack or QR
                                     cooldown end), or nil. Guarded by mu
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
    Instant          bool            cast by an instant_cast powerup (no scans)
    Boosted          bool            a damage_multiplier multiplied Dealt
    ShortQR          bool            a short_qr_cooldown was active: cools for its time
  Boss       BossProgress            a value: copied with the State
    Defeated    int                  bosses defeated; current boss = index Defeated
    Damage      int                  dealt to the current boss so far (< its HP)
    Multiplier  float64              casts' damage × this, rounded; 1 by default
    PhaseStart  time.Time            when the current boss's phase began (stored)
    PhaseHealth int                  health when it began (stored)
    Attacks     int                  attacks this phase, as of the last catch-up (derived)
    Health      int                  health as of the last catch-up (derived); 0 = knocked out
  QRCooldowns []QRCooldown           waypoints the team can't scan, soonest Until first;
                                     derived from Casts and the clock; never modified
    Waypoint  int                    the waypoint's index in the atlas
    Until     time.Time              when it opens: its latest cast's At + game.qr_cooldown
  CooldownsReset int                 the cast log's length at an admin's last cooldown reset:
                                     only Casts[CooldownsReset:] count for cooldowns; 0 if never
  Powerups   Powerups                a value: copied with the State
    Held            PowerupSet uint8 the kinds held, bit k = kind k (stored)
    MultiplierArmed bool             the next cast deals × powerups.damage_multiplier
                                     (derived: team_powerup.activated_casts == len(Casts))
    ShortQRUntil    time.Time        the short_qr_cooldown window's end; zero if none
                                     (derived: activated_at + powerups.short_qr_window)
  QTEAnswers []QTEAnswer             the team's quiz answers, oldest first; copy-on-write
    QTE, Option, Correct, Granted, By, At   one team_qte_answer row
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
- **Size:** a `State` is a fixed **1440 bytes** (`Stack` 912, `Selection` 56, the `Casts`, `QRCooldowns` and `QTEAnswers` slice headers 24 each, `Boss` 72, `CooldownsReset` 8, `Powerups` 32; measured with `unsafe.Sizeof`, 64-bit), so a new snapshot is a plain struct copy. The cast log's backing array is on the heap, **80 bytes per cast**; the QR cooldowns' **32 bytes per cooling waypoint** (at most the waypoints of the casts of the last 5 minutes); the answers' **64 bytes per quiz answered**. 10,000 teams' states are about 13.7 MB, plus 0.8 MB for every 1 cast per team on average (20 casts each: about 16.5 MB). Each started team also holds one pending timer (a `time.Timer`, about 100 bytes).
- **Why health is derived, not stored.** Storing it would make every attack a database write: up to one per team per boss interval, all day, with nobody acting, and a write that can fail with no player to report it to. Deriving it from the phase (`PhaseStart`, `PhaseHealth`) and the clock makes an attack a memory-only change that `Load` reproduces exactly: only a defeat and a revive, which a player or admin caused, write.
- **Why the cast log is a slice and copy-on-write.** Its length isn't bounded, so it can't be a fixed array. Snapshots share the backing array until a cast: `Scan` builds the new log with `append(slices.Clip(cur.Casts), cast)`, which always copies, so a reader holding the old snapshot keeps seeing the old log. A cast costs one copy of the log (80 bytes per earlier cast), and only casts pay it: selections and scans copy the slice header only.
- **Why QR cooldowns are a field, not computed on every check.** `Scan` checks them on every scan and `view` shows them on every publish; deriving them each time would walk the cast log. They're re-derived only when they change (a cast, an expiry) and at load, and replaced, never modified, so snapshots share them like `Casts`.
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
| `team_cast` | `0007`, `0009`, `0014` | `id` BIGINT identity PK, `team_id` VARCHAR(6), `pattern_id` INTEGER, `cast_by` UUID, `cast_at` TIMESTAMPTZ, `already_completed` BOOLEAN, all `NOT NULL`; from `0009`: `dealt` INTEGER `NOT NULL DEFAULT 0 CHECK (dealt >= 0)`, `boss_serial` INTEGER NULL → `boss (serial)`, `defeated_boss` BOOLEAN `NOT NULL DEFAULT false` | One row per **cast**, repeats included (unlike `team_pattern`): the cast log. `pattern_id` → `pattern` (a cast is of a real pattern); `cast_by` → `userprofile` (a real user); `already_completed` records whether `team_pattern` already had the pattern. `id` orders casts: `Load` reads them `ORDER BY team_id, id`. Index `team_cast_team_id_idx` on `(team_id, id DESC)` serves a one-team read (`reconcile`). No unique constraint: any number of casts per team and pattern. `boss_serial` → `boss` (a cast hits a real boss); `NULL` for a cast after every boss was defeated, or from before `0009` (whose `dealt` defaults to 0) |
| `team_powerup` | `0014` | `team_id` VARCHAR(6), `kind` TEXT `CHECK (in the four kinds)`, `held` BOOLEAN `DEFAULT false`, `activated_at` TIMESTAMPTZ NULL, `activated_casts` INTEGER NULL `CHECK (>= 0)` | PK `(team_id, kind)`: **at most one of each kind held**. `activated_casts` only for a `damage_multiplier`, and only with `activated_at` (a CHECK). No row: never held. `team_cast` gains `instant`, `boosted`, `short_qr_cooldown` (`0014`, `DEFAULT false` for older casts) |
| `team_qte_answer` | `0015` | `team_id` VARCHAR(6), `qte_id` BIGINT → `qte`, `option` INTEGER `CHECK (>= 0)`, `correct`, `granted` BOOLEAN, `answered_by` UUID → `userprofile`, `answered_at` TIMESTAMPTZ, all `NOT NULL` | PK `(team_id, qte_id)`: **one answer per team per QTE**. `CHECK (correct OR NOT granted)`. The QTE itself is the qte use case's table ([`internal/qte/README.md`](../qte/README.md)) |
| `team_boss` | `0009`, `0010`, `0013` | `team_id` VARCHAR(6) PK, `defeated` INTEGER `DEFAULT 0 CHECK (>= 0)`, `damage` INTEGER `DEFAULT 0 CHECK (>= 0)`, `multiplier` DOUBLE PRECISION `DEFAULT 1 CHECK (> 0)`, all `NOT NULL`; from `0010`: `phase_health` INTEGER NULL `CHECK (>= 0)`, `phase_started_at` TIMESTAMPTZ NULL; from `0013`: `cooldowns_reset_casts` INTEGER `NOT NULL DEFAULT 0 CHECK (>= 0)` (the cast log's length at an admin's last cooldown reset; 0: never) | One row per team with boss progress (PK `team_id`); no row means boss 1, 0 damage, multiplier 1, a phase from the team's start at full health. `phase_health` `NULL` means full health (`game.health`), `phase_started_at` `NULL` the team's `started_at`. Written by `insertCompletingScan`'s transaction (every cast) and by `Revive` (`upsertBossPhase`), both through `upsertBoss`. Health and attacks aren't columns: they're derived. The CHECKs back "progress never negative", "the multiplier is positive" and "health never negative". That `defeated` ≤ `game.bosses` and `damage` < the boss's HP are checked in memory (Load, `deal`), since they depend on config and another table |

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
| `team_boss` | `State.Boss` (`Defeated`, `Damage`, `Multiplier`, `PhaseHealth`, `PhaseStart`) and `State.CooldownsReset` (`cooldowns_reset_casts`); no row → boss 1, 0, multiplier 1, full health from the team's `started_at`, never reset |
| `team.started_at` (through `Teams.ByID`) | `PhaseStart` when `phase_started_at` is `NULL` or there's no row |
| (none: derived from the phase, `game.boss_attack.interval`, the boss's `game.boss_attack.damage` and the clock) | `BossProgress.Attacks`, `Health`: recomputed by `at(now)` at `Load`, so attacks that fell due while the backend was down are applied at once |
| (none: derived from `team_cast`, `game.qr_cooldown` and the clock) | `State.QRCooldowns`: every waypoint of a cast with `cast_at` + `game.qr_cooldown` after now, the latest such cast per waypoint; recomputed at `Load`, so cooldowns that ended while the backend was down are gone |
| `team_powerup` | `Powerups.Held`; `MultiplierArmed` (`activated_casts` = the log's length; more than its length refuses the load); `ShortQRUntil` (`activated_at` + `powerups.short_qr_window`, if still ahead) |
| every `team_qte_answer` row, `ORDER BY team_id, answered_at, qte_id` | `State.QTEAnswers`, oldest first; `qte` is its last |
| `team_cast.short_qr_cooldown` | each cast's QR cooldown length in `qrCooldowns` (config applies retroactively, like `game.qr_cooldown`) |
| (none: memory-only) | `entry.stopTimer` (the pending timer): re-armed by `Load` for every started team |
| `boss` | Not here: the bosses use case, loaded by `bosses.Load` before this `Load` ([`internal/bosses/README.md`](../bosses/README.md)) |
| newest **7** (`game.max_pattern_points` + 1) `team_scan` with `id > after_scan_id`, by `id` | `State.Stack` (pushed oldest first, with the `wrong` flag, so `poisoned` comes back too) |
| (none: built from the atlas and `config.toml`) | `Store.game`, rebuilt by `newGame` at every `Load` |
| (none: memory-only, from teams) | `entry.members`, the roster cache: refilled from `Teams.ByID` on each team's first publish after a restart. teams reloads rosters from `teammember` (an admin's removal deleted the row), and `RosterChanged` drops the cache on every removal, so the cache always matches |
| (none: memory-only) | `State.Version`: `Load` starts every team at seq 0 in the new process's epoch, which clients treat as newer than anything from the old process, so nothing needs storing |

**Nothing else is held only in memory.** The timers are rebuilt by `Load`, and health and QR cooldowns are functions of what's stored and the clock, so a restart reproduces memory exactly at the same moment, versions aside: `TestRestartRestoresEverything`, `TestCastLog`, `TestCastsDamageBosses` and `TestAttacksAcrossRestart` compare the JSON before and after.

**Casts before migration `0007` aren't logged**, so a team whose only casts predate it reloads with an empty log and `last_cast: null` although its `patterns` are non-empty. Every cast since is logged, in the same order memory appended it (`id` is an identity column, and casts for one team are written under its entry lock), so a restart reproduces the log exactly.

## Wrong scans and poisoned attempts
How a mistake plays out, end to end.

**The server decides.** In `Scan`, under the team's lock, after the rejections (no selection, poisoned, waypoint already in): `wrong := !FitsShapeCastable(selectedShape, stack ∪ {w}, unlockedPoints, cooldown)`. `FitsShapeCastable` is true if some pattern of the shape that the team can cast now (at most `unlockedPoints` waypoints, not of the cooldown class) contains every waypoint in the set ([`internal/atlas/README.md`](../atlas/README.md)).

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
2. Otherwise `dealt = math.Round(pattern damage × Multiplier)` (halves away from zero: 27 × 1.5 = 40.5 → 41), added to `Damage`.
3. If `Damage` reaches the current boss's HP (≥): `Defeated` + 1, `Damage` = 0. **The excess is lost**, never carried to the next boss, so one cast defeats at most one boss. The event type is `boss_defeated`. The next boss's **phase starts at the cast**, with the health the team has then: its attack clock starts from zero.
4. The cast records `dealt`, `boss` (the serial hit) and `defeated_boss`; the progress is upserted into `team_boss` in the cast's transaction.

**Repeats count.** A cast of an already-completed pattern (`already_completed: true`) deals damage like any other.

**Defeating bosses unlocks bigger patterns** (`game.unlocks`): `unlocked_points` goes from 3 to 4 at boss 2's defeat (`quadrilateral` leaves `locked_shapes`), to 5 at boss 4's (`pentagon`) and to 6 at boss 5's (`hexagon`; `locked_shapes` empties), each in the same `boss_defeated` event. So bosses 1 and 2 are fought with lines and triangles only: boss 1 (25 HP) falls to any cast, boss 2 (100 HP) to two triangles or a triangle and lines (the class cooldown forbids two triangles in a row); quadrilaterals arrive for boss 3.

**The multiplier** is stored per team (`team_boss.multiplier`) and applied to every cast, but **nothing sets it**, and a `damage_multiplier` powerup multiplies one cast on top of it: it's 1 for every team unless changed in Postgres with the backend stopped. A future powerup would change it as a teamstate write. A cast deals at most 2³¹ − 1 (`maxDealt`, the INTEGER columns it writes): an absurd multiplier is clamped there rather than failing every cast, and the clamped cast still defeats its boss.

**What the client shows** (`/play`): the current boss from `GET /api/game/bosses` by `boss.serial` (image, name, description), an HP bar of `hp − boss.damage` of `hp`, the multiplier when it isn't 1, "Every boss defeated!" when `boss` is `null`; a `boss_defeated` event shows "<name> defeated!" on every phone; the cast log shows each cast's `dealt` and the boss it hit.

## Boss attacks and health
Every team has health, **10** (`game.health`) at the start. Its current boss attacks it every **10 min** of the phase (`game.boss_attack.interval`, the same for every boss), for its damage (`[game.boss_attack.damage]` by serial: boss 1 does 1 … boss 6, Gideon, does 6, for now). Both come from `config.toml` through `bosses.Rules`, filled into each `bosses.Boss` (`AttackInterval()`, `AttackDamage`). At full health a team stalled on boss 1 lasts 100 min (10 attacks); on the twins or Gideon, 20 min (2 attacks); health carries over between bosses. It's one of the game's two changes caused by time passing (the other is a [QR cooldown](#qr-cooldown) ending), so the server applies and publishes it with nobody acting (AGENTS.md §9: the server owns time).

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
1. **The timer.** `schedule` arms `Clock.AfterFunc` for `nextAttack` = `PhaseStart + (attacks + 1) × interval` (or a QR cooldown's end, if sooner). When it fires, `due` locks the entry, calls `catchUp` and re-arms.
2. **`catchUp`** computes `at(now)`; if anything fell due it bumps the version, applies, and publishes `boss_attacked`, or `knocked_out` if health hit 0. No persist.
3. **Every write catches up first.** `Select`, `ClearSelection`, `Scan` and `Revive` call `catchUp` right after taking the lock. So a timer that's late (a busy process, or the lock held by a write) never loses an attack: the write applies it first, with its own event, and then validates against the real health. A cast that defeats a boss is dealt after the attacks due before it.
4. **Several due attacks are one change.** A late timer, or a restart after downtime, applies every attack due as one `boss_attacked` (or `knocked_out`) event, with `boss.attacks` up by that many.

**Knocked out** (health 0): `Select` and `Scan` are refused with `409 knocked_out` (nothing written, no event); `ClearSelection` still works; `nextAttack` is not ok, so no timer is armed and nothing attacks. The progress, patterns, cast log and selection are kept.

**Revive** (`POST /api/admin/teams/{team_id}/revive`, admin only): health back to 10 and a new phase of the same boss from now (its earlier attacks no longer count); `revived` to every member; the timer re-armed. Allowed at any time, so it also heals a team that isn't knocked out.

**Config changes apply retroactively.** Health is recomputed from the phase start, so `game.boss_attack.interval` or a boss's damage changed in `config.toml` between restarts changes every current phase against it (see [Not decided yet](#not-decided-yet)).

**What the client shows** (`/play`): a health bar of `health` of `Game.health`; the current boss's interval and damage, and a countdown to `boss.next_attack_at` on the server's clock (`hello`, `ServerClock`); when `knocked_out`, a red banner and the type buttons and scan form disabled; on `boss_attacked`, `knocked_out` and `revived`, a message on every phone. The client only displays: the server applies attacks.

## QR cooldown
For **5 min** (`game.qr_cooldown`) after a cast, the waypoints of its pattern are disabled for the team that cast it. Nobody on the team can scan them; other waypoints stay open, and other teams aren't affected. So a team can't stand at the same four QR codes and cast over and over: it has to move.

**The rule.**
- A cast at *T* puts each waypoint of its pattern on cooldown until *T* + `game.qr_cooldown`. A waypoint in several recent casts takes its latest cast's (possible only if `game.qr_cooldown` was raised between restarts: while it's enforced, a cooling waypoint can't be in a new cast).
- A waypoint is cooling while `now < until`. At exactly `until` it opens again.
- `Scan` refuses a cooling waypoint with `409 waypoint_on_cooldown`: nothing written, pushed or published, so the attempt isn't spoiled. The check comes after `waypoint_already_scanned` and before the wrong-scan check.
- **Not affected:** selecting (a team may select a shape whose patterns all need cooling waypoints: it just can't scan those yet, and there's no time limit); wrong scans, clears and failed casts (only a completed cast starts a cooldown); revives and boss defeats (neither clears it).
- `game.qr_cooldown = 0` turns it off: `qr_cooldowns` is always `[]`.

**Derived, never stored.** The cast log already holds each cast's time and pattern, so `qrCooldowns(Casts, now)` rebuilds the list: newest cast first, each of its waypoints not seen yet, until the first cast whose cooldown has ended. `Load` and `reconcile` (`stateOf`) call it, so a restart reproduces it exactly and drops what ended while down; `Scan` calls it on a cast.

**Expiry is published by the server.** It's a change caused by time passing (AGENTS.md §9), handled exactly like an attack:
1. **One timer per team** for every time-driven change: `schedule` arms it for the earliest of the next attack and the soonest `until`.
2. **`catchUp`** finds the soonest `until` ≤ now, re-derives the list, bumps the version, applies and publishes `qr_cooldown_ended`. No persist. If an attack fell due at the same moment, the one event is `boss_attacked` (or `knocked_out`), carrying both changes.
3. **Every write catches up first**, so a late timer never keeps a waypoint closed: a scan at `until` first applies the expiry (`qr_cooldown_ended`), then is accepted.

| Time | What happens | `qr_cooldowns` after | Event |
|---|---|---|---|
| 12:00:12 | A's scan casts the quadrilateral `2001 2002 2007 2008` | 2001, 2002, 2007, 2008, each until 12:05:12; timer armed for 12:05:12 (sooner than the 12:10:00 attack) | `spell_cast` (or `boss_defeated`) |
| 12:01:00 | F selects `triangle` | unchanged | `shape_selected` |
| 12:01:10 | B scans `2001` | unchanged | `409 waypoint_on_cooldown`, none |
| 12:01:20 | B scans `2003` | unchanged | `scan_added` |
| 12:05:11.999999 | (nothing due) | unchanged | none |
| 12:05:12 | the timer: `catchUp` finds the cooldowns ended | `[]`; timer armed for the 12:10:00 attack | `qr_cooldown_ended` |
| 12:05:30 | B scans `2001`, C scans `2002`: the triangle casts | 2001, 2002, 2003, each until 12:10:30 | `scan_added`, `spell_cast` |

**A restart** at 12:03:00 reloads the quadrilateral's cast from `team_cast` and rebuilds the same four entries; one at 12:06:00 finds them ended and loads `[]`.

**What the client shows** (`/play`, not built yet): each cooling waypoint with a countdown to its `until` on the server's clock (`hello`), and scanning it disabled; on `qr_cooldown_ended` (or any state without it), enabled again. The countdown only displays: the waypoint opens when the server's event arrives.

## Powerups
Temporary effects a team **holds** (at most one of each kind) and its **leader** uses. An admin grants them (`POST /api/admin/teams/{team_id}/powerups/{kind}`, `GrantPowerup`), and a right quiz answer earns one ([Earned by quizzes](#earned-by-quizzes)).

| Kind | Using it | Ends |
|---|---|---|
| `damage_multiplier` | Arms the next cast: it deals `round(damage × team_boss.multiplier × 2)` (`powerups.damage_multiplier`), marked `boosted` | Used up by that cast (an instant cast included) |
| `heart` | `PhaseHealth` + `min(1, game.health − health)` (`powerups.heart`): the attacks so far still count, so health rises by that and `next_attack_at` doesn't move. Refused at full health or knocked out | At once |
| `short_qr_cooldown` | For **5 min** (`powerups.short_qr_window`), casts are marked `short_qr_cooldown` and cool their waypoints for **2 min** (`powerups.short_qr_cooldown`) | By itself, at the window's end (`powerup_ended`) |
| `instant_cast` | The named pattern is cast at once, with no scans, **past** the class and QR cooldowns (not past the unlock); then a cast like any other | At once |

**Stored as moments, the rest derived**, like attacks and QR cooldowns: `team_powerup` holds what's held and each kind's last activation; an armed multiplier is a position in the cast log (`activated_casts`), so the cast that uses it writes nothing extra; a window is `activated_at` + config, so its end writes nothing; each cast's flags say what powerups did to it. `Load` rebuilds it all, and the team's one timer also fires at a window's end.

A waypoint in several casts cools until the **latest** end: only an instant cast can cast a cooling waypoint, so for scanned casts this is the old "latest cast decides".

### Earned by quizzes
An admin's quiz (the [`qte`](../qte/README.md) use case: content, start, close, the game-wide `qte` event) names a powerup. **Each team answers once**: any member, and the first answer is the team's (`team_qte_answer`'s primary key). It's marked right or wrong **at once**, and a right one earns the powerup **in the same transaction** (`holdPowerup`), unless the team already holds that kind (`granted: false`: wasted, as with `GrantPowerup`). `team_state.qte` is the latest answer; it's built from `State` alone, so a QTE starting or closing changes no team's state. Refused once the QTE's `ends_at` has passed (the stored moment, not the close timer, decides), and while knocked out.

## Walkthroughs
Team `482913`, members A–F, **F the leader** (only F selects and clears), 2026 seed (quadrilateral = pattern 188, `2001 2002 2007 2008` = indexes 0, 1, 6, 7, bitmask 195, damage 274; `quadrilateral` = shape 3), config.toml as shipped. In walkthroughs 1–6 the team has already defeated **2** bosses, so quadrilaterals are unlocked (`unlocked_points` 4); walkthroughs 7 and 8 start from the first boss. Times are `mm:ss` after 12:00. Stacks are written bottom → top as `waypoint/member@time`; a wrong scan is marked ✗.

### 1. A quadrilateral, with every rejection
| Time | Request | Check | Stack after | Postgres after | Response |
|---|---|---|---|---|---|
| 00:00 | A: select `quadrilateral` | **A isn't the leader** | empty | unchanged | `403 not_leader` |
| 00:00 | F: select `quadrilateral` | team IN_GAME, shape known, F leads, `quadrilateral` (4 points) not locked | empty | `team_selection(quadrilateral, F, after_scan_id = latest)` | `200`, `selection.shape = "quadrilateral"` |
| 00:01 | A: scan `2001` | `{2001}` (bitmask 1) is inside some quadrilateral | `2001/A@01` | `team_scan` +1 | `scan_added` |
| 00:02 | A: scan `2001` (a retry) | **2001 is already in the stack** | unchanged | unchanged | `409 waypoint_already_scanned` |
| 00:03 | B: scan `2001` | **2001 is already in the stack** | unchanged | unchanged | `409 waypoint_already_scanned` |
| 00:05 | A: scan `2002` | the same member again is fine; `{2001,2002}` = 3 is inside quadrilateral 195; 3 is also a **line** (shape 1 ≠ 3), so no completion | `2001/A 2002/A@05` | `team_scan` +1 | `scan_added` |
| 00:10 | C: scan `2007` | set 67 fits the quadrilateral; 67 is also a **triangle** (shape 4 ≠ 3), so no completion | `… 2007/C@10` | `team_scan` +1 | `scan_added` |
| 00:12 | A: scan `2008` | set 195 = quadrilateral, shape 3 = selection | **purged**; selection cleared; cast log + quadrilateral 195 by A @12 | `team_scan` +1, `team_pattern` +1, `team_cast` +1, `team_selection` deleted | `pattern_completed`, `pattern ["2001","2002","2007","2008"]`, `last_cast.cast_by` = A |
| 00:13 | A: scan `2008` (a retry) | no selection | unchanged | unchanged | `409 no_shape_selected` |

### 2. A wrong scan poisons the attempt, and a reselect restarts it
With a test waypoint `2099` in no pattern (the 2026 content has none, so with it alone every checkpoint scan here fits).

| Time | Request | Check | Stack after | `poisoned` | Response |
|---|---|---|---|---|---|
| 00:00 | F: select `quadrilateral` | | empty | false | `200` |
| 00:01 | A: `2001` | inside a quadrilateral | `2001/A@01` | false | `scan_added` |
| 00:02 | B: `2002` | inside a quadrilateral with 2001 | `2001/A 2002/B@02` | false | `scan_added` |
| 00:03 | C: `2099` | no quadrilateral has 2001, 2002 *and* 2099 | `2001/A 2002/B 2099/C@03 ✗` | **true** | `200 wrong_scan`; every phone gets the `team_state` |
| 00:04 | D: `2007` | **poisoned** | unchanged | true | `409 selection_poisoned` |
| 00:05 | C: `2099` (a retry) | **poisoned** (checked before "already in") | unchanged | true | `409 selection_poisoned` |
| 00:06 | F: select `quadrilateral` | reselect always purges | empty (2099 gone) | false | `200` |
| 00:07–10 | A, B, C, D: `2001 2002 2007 2008` | exact quadrilateral | **purged** | false | `pattern_completed` |

Postgres after 00:03: `team_scan` row for `2099` with `wrong = true`. A restart between 00:03 and 00:06 reloads the same poisoned stack. `2099` stays in visited `waypoints` after the purge.

### 3. No time limit
F selects `quadrilateral` at 00:00. A scans `2001` at 00:00, B `2002` at 01:00:00 (an hour later), C `2007` at 02:00:00. At 10:00:00 the stack is still `2001/A@00:00 2002/B@01:00:00 2007/C@02:00:00`: nothing ages out, no event is sent with nobody acting. D scans `2008` at 10:00:00: `pattern_completed`.

### 4. Reselecting purges
F selects `quadrilateral`; A, B, C scan `2001 2002 2007`. F selects `quadrilateral` again: the stack is purged (`after_scan_id` moves past those three rows). D scans `2008`: `scan_added`, stack `2008/D` only. A may scan `2001` at once (the earlier scan of it is gone).

### 5. One member scans a whole pattern
F selects `quadrilateral`. D scans `2001`, `2002`, `2007`, `2008` in turn: `scan_added` three times, then `pattern_completed`, `last_cast.cast_by` = D.

### 6. The cast log after a cast and a repeat
| Time | Request | `last_cast` after | `GET /api/teams/me/casts` | `patterns` | `team_cast` rows | `team_pattern` rows |
|---|---|---|---|---|---|---|
| (start) | | `null` | `[]` | `[]` | 0 | 0 |
| 00:12 | A completes quadrilateral `2001 2002 2007 2008` (walkthrough 1) | `{"pattern":["2001","2002","2007","2008"],"shape":"quadrilateral","class":"class_4","cast_by":A,"cast_at":"…12:00:12Z","already_completed":false,"damage":274,"dealt":274,"boss":3,"defeated_boss":true}` (274 ≥ boss 3's 250 HP) | `[that cast]` | `[[2001,2002,2007,2008]]` | 1 | 1 |
| 01:00 | F selects `triangle` | **unchanged** (a selection doesn't clear it) | unchanged | unchanged | 1 | 1 |
| 02:00 | F selects `quadrilateral` again | unchanged | unchanged | unchanged | 1 | 1 |
| 02:01–02:04 | A, B, C, D scan the same quadrilateral; D's scan completes it | `{…,"cast_by":D,"cast_at":"…12:02:04Z","already_completed":true,"dealt":274,"boss":4,"defeated_boss":false}` (274 < boss 4's 510 HP) | `[02:04 cast (already_completed: true), 00:12 cast]` | unchanged (counted once) | 2 | 1 (`ON CONFLICT DO NOTHING`) |
| (restart) | | the 02:04 cast | the same two, same order | unchanged | 2 | 1 |

`ScanResult.already_completed` and `last_cast.already_completed` agree for the scan that cast.

### 7. Casts through the first bosses (seed HP 25, 100, 250, 510; quadrilateral 188 deals 274, triangle 46 (`2001 2002 2003`) 68, line 1 (`2001 2002`) 27; multiplier 1)
Until boss 2 falls the team can cast only lines and triangles (`unlocked_points` 3), and the cooldown forbids two of a class in a row, so it alternates:

| Cast | `boss` before | `dealt` | `boss` after | `bosses_defeated` | `unlocked_points` after | `team_boss` after (defeated, damage, multiplier) | Event type |
|---|---|---|---|---|---|---|---|
| 00:12 triangle | `{serial 1, damage 0}` | 68 | `{serial 2, damage 0}` (68 − 25 = 43 lost) | 1 | 3 | `(1, 0, 1)` (row inserted) | `boss_defeated` |
| 01:00 line | `{serial 2, damage 0}` | 27 | `{serial 2, damage 27}` | 1 | 3 | `(1, 27, 1)` | `spell_cast` |
| 02:04 triangle | `{serial 2, damage 27}` | 68 | `{serial 2, damage 95}` | 1 | 3 | `(1, 95, 1)` | `spell_cast` |
| 03:00 line | `{serial 2, damage 95}` | 27 | `{serial 3, damage 0}` (22 lost) | 2 | **4** (`quadrilateral` leaves `locked_shapes`) | `(2, 0, 1)` | `boss_defeated` |
| 04:00 quadrilateral | `{serial 3, damage 0}` | 274 | `{serial 4, damage 0}` (24 lost) | 3 | 4 | `(3, 0, 1)` | `boss_defeated` |

With `team_boss.multiplier = 1.5` instead, the triangle deals 102, the line 41 and the quadrilateral 411: the first triangle defeats boss 1, a line and a triangle boss 2 (143 ≥ 100), and so on. Boss 1 falls to any cast, boss 2 to two triangles or a triangle and lines, and one quadrilateral defeats boss 3 alone.

After the sixth boss (Gideon) falls, `boss` is `null`, `bosses_defeated` 6, and further casts have `dealt 0`, `boss null`, type `spell_cast`. Boss 4's defeat unlocks pentagons (`unlocked_points` 5) and the fifth (the Katayanagi twins) hexagons: from that `boss_defeated` event on, `unlocked_points` is 6 and `locked_shapes` is `[]`.

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
| 13:03:00 | F selects `triangle` | 0 | unchanged | unchanged | `409 knocked_out`, none |
| 13:03:05 | F clears the selection (allowed while knocked out) | 0 | unchanged | unchanged | `200`, a no-op here (the cast had cleared the selection) |
| 13:10:00 | admin revives | **10** | `{serial 2, attacks 0, phase_started_at 13:10:00, next_attack_at 13:20:00}`; timer re-armed | `(1, 0, 1, phase_health 10, phase_started_at 13:10:00)` | `revived` |

**A restart at 12:35:00** (between the 12:32:30 attack and the next) reloads `phase_health 8, phase_started_at 12:22:30` from `team_boss`, computes `attacks = floor(750 s / 600 s) = 1`, `health = 8 − 1 × 2 = 6`, the same as before, and arms the timer for 12:42:30. **A backend down from 12:35:00 to 12:55:00** reloads with `attacks = floor(1950 / 600) = 3`, `health 2`: the two attacks it missed are applied at load, and clients see them in their `snapshot`.

**A late timer.** If the 12:10:00 timer hasn't run when a scan arrives at 12:10:00.2, the scan's `catchUp` applies the attack first (`boss_attacked`), then the scan (`scan_added`): two events, in that order. The timer then finds nothing due and re-arms for 12:20:00.

## The write-through
Every write in `write.go` (`Select`, `ClearSelection`, `Scan`, `Revive`, `ResetCooldowns`) runs these steps, marked in the code (`PublishTeam` is not a write, and `catchUp` is a memory-only change; see the table):
1. **lock** the team's entry (`lockTeam`)
2. **validate**:
   - first the atlas lookup (shape or waypoint ID) and the team, **before** the lock (an `IN_GAME` team's status never goes back). This is AGENTS.md §6 variation (a); the code labels it `2. validate, before 1. lock`
   - then, under the lock, membership again (`isMember`: an admin may have removed the caller since the lookup), then `catchUp` (any due boss attack or QR cooldown end, its own change), then the `State` (`2. validate (state)`: knocked out first for `Select` and `Scan`)
3. **build** the next `State` (a copy; the cast log copy-on-write)
4. **persist**: one `db.go` call (`insertScan`, `insertCompletingScan`, `upsertSelection`, `deleteSelection`, `upsertBossPhase` or `upsertCooldownsReset`), detached from the request and capped at **5 s** (`storage.write_timeout`)
5. **apply**: `entry.cur.Store(&next)`
6. **publish** `team_state` to every member, still holding the lock so a team's events are in order; a cast, revive or cooldown reset then re-arms the team's timer (`schedule`). A cast that defeated the last boss calls `Rules.OnFinished` after the lock is released

| Write | `db.go` call (step 4) |
|---|---|
| `Select` | `upsertSelection` |
| `ClearSelection` | `deleteSelection` |
| `Scan`, no completion (wrong included) | `insertScan` |
| `Scan` that casts | `insertCompletingScan` (`team_scan`, `team_pattern`, `team_cast`, upsert `team_boss`, delete `team_selection`, one transaction) |
| `Revive` (admin) | `upsertBossPhase` |
| `ResetCooldowns` (admin) | `upsertCooldownsReset` |
| `GrantPowerup` (admin) | `upsertPowerupHeld` |
| `Activate` | `updatePowerupActivated`; a heart: `useHeart` (`team_powerup`, upsert `team_boss`, one transaction) |
| `InstantCast` | `insertInstantCast` (`team_powerup`, then `team_pattern`, `team_cast`, upsert `team_boss`, delete `team_selection`, one transaction) |
| `AnswerQTE` | `insertQTEAnswer` (`team_qte_answer`, then `team_powerup` if granted, one transaction) |
| `TeamDeleted` (teams' delete hook) | none: **memory-only** (AGENTS.md §6); teams' `deleteTeam` already removed the rows. Drops the entry and stops its timer; publishes nothing |
| `catchUp` (the team's timer, and the start of every write) | none: **memory-only** (AGENTS.md §6). Health is derived from the stored phase and QR cooldowns from the cast log, so `Load` reproduces the change. Steps 3, 5 and 6 |
| `PublishTeam` (start hook) | none: not a write. Only steps 2, 1 and 6; the state is published as it is, then the timer is armed |

**On a failed persist:** `reconcile` reloads the team from Postgres (Postgres wins), publishes the result, and returns the error, which becomes a `500`. Nothing from the failed write is applied.

**Rejections and no-ops return before step 4:** every `409` (`selection_poisoned`, `knocked_out` and `waypoint_on_cooldown` included) and clearing when there's nothing to clear. A rejection after a `catchUp` that applied an attack or an expiry still leaves it applied and published: it's its own change. Nothing is written and nothing is published. A **wrong scan is not a rejection**: it's written and published. Reselecting is **not** a no-op: it always purges, writes and publishes.

## API
All routes need `Authorization: Bearer <token>` (`401` without). Bodies over **1 MiB** (`server.max_body_bytes`) get `400 invalid_body`.

| Method | Path | Body | Response payload |
|---|---|---|---|
| GET | `/api/game/shapes` | none | `Game` |
| GET | `/api/teams/me/state` | none | `View` |
| GET | `/api/teams/me/casts` | none | `[CastView]`, newest first |
| PUT | `/api/teams/me/state/selection` | `{"shape": "quadrilateral"}` | `View` |
| DELETE | `/api/teams/me/state/selection` | none | `View` |
| POST | `/api/teams/me/state/scans` | `{"qr": "2007:1760000000.nR5…"}` | `ScanResult` |
| POST | `/api/teams/me/state/powerups/{kind}` | none; `instant_cast`: `{"pattern": ["2001","2002","2007","2008"]}` | `View` |
| POST | `/api/teams/me/state/qte/{qte_id}/answer` | `{"option": 2}` | `View` |

**`View`** (also the data of the `team_state` live event), here for a poisoned attempt (through `2099`, a test waypoint in no pattern: the 2026 content can't poison one):
```json
{
  "team_id": "482913",
  "waypoints": ["2001", "2002", "2003", "2004", "2005", "2099"],
  "patterns": [["2003", "2004", "2005"]],
  "shapes": ["triangle"],
  "selection": {"shape": "quadrilateral", "selected_by": "6f1c…", "selected_at": "2026-10-17T12:00:00Z"},
  "last_cast": {"pattern": ["2003", "2004", "2005"], "shape": "triangle", "class": "class_3", "cast_by": "9a2b…", "cast_at": "2026-10-17T11:58:40Z", "already_completed": false, "damage": 66, "dealt": 66, "boss": 3, "defeated_boss": false},
  "stack": [
    {"waypoint_id": "2001", "user_id": "9a2b…", "scanned_at": "2026-10-17T12:00:01Z", "wrong": false},
    {"waypoint_id": "2002", "user_id": "c3d4…", "scanned_at": "2026-10-17T12:00:02Z", "wrong": false},
    {"waypoint_id": "2099", "user_id": "e5f6…", "scanned_at": "2026-10-17T12:00:03Z", "wrong": true}
  ],
  "poisoned": true,
  "bosses_defeated": 2,
  "boss": {"serial": 3, "damage": 66, "phase_started_at": "2026-10-17T11:45:00Z", "attacks": 1, "next_attack_at": "2026-10-17T12:05:00Z"},
  "damage_multiplier": 1,
  "health": 9,
  "knocked_out": false,
  "unlocked_points": 4,
  "locked_shapes": ["hexagon", "pentagon"],
  "cooldown_class": "class_3",
  "cooldown_shapes": ["triangle"],
  "qr_cooldowns": [],
  "cast_count": 1,
  "powerups": {"held": ["heart"], "active": [{"kind": "short_qr_cooldown", "until": "2026-10-17T12:04:00Z"}]},
  "qte": {"qte_id": 7, "option": 2, "correct": true, "granted": true, "answered_by": "c3d4…", "answered_at": "2026-10-17T11:59:12Z"},
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
- `powerups.held` are the kinds the team holds, `powerups.active` the effects running (`until` null for an armed `damage_multiplier`, the window's end for a `short_qr_cooldown`); both `[]` when empty. A cast carries `instant`, `boosted` and `short_qr_cooldown`.
- `qte` is the team's latest quiz answer, or `null` before its first: the option chosen, whether it was `correct`, whether it `granted` the QTE's powerup (false when wrong, or the kind was already held), who answered and when. Compare `qte_id` with the `qte` event's `id` to show "answered" for the open quiz.
- `qr_cooldowns` are the waypoints the team can't scan now (`409 waypoint_on_cooldown`): `{"waypoint_id": "2001", "until": "2026-10-17T12:05:12Z"}` for each waypoint of a cast less than **5 min** (`game.qr_cooldown`) ago, sorted by `until`, then ID; `[]` when none (above: the last cast was at 11:58:40, so its cooldown ended at 12:03:40). An entry leaves at its `until`, with a `qr_cooldown_ended` event.
- `unlocked_points` is the most waypoints a pattern the team can cast may have, by the unlock schedule (`game.unlocks`): **3** at the start, **4** after boss 2, **5** after boss 4, **6** after boss 5 (`game.max_pattern_points`). `locked_shapes` are the shapes it can't select yet (every pattern bigger), in index order; with the 2026 seed `["hexagon", "pentagon", "quadrilateral"]` at the start, `["hexagon", "pentagon"]` after boss 2 (as above), `["hexagon"]` after boss 4, `[]` after boss 5.

**Cast log** (`GET /api/teams/me/casts`), after walkthrough 6:
```json
[
  {"pattern": ["2001", "2002", "2007", "2008"], "shape": "quadrilateral", "class": "class_4", "cast_by": "<D>", "cast_at": "2026-10-17T12:02:04Z", "already_completed": true, "damage": 274, "dealt": 274, "boss": 4, "defeated_boss": false},
  {"pattern": ["2001", "2002", "2007", "2008"], "shape": "quadrilateral", "class": "class_4", "cast_by": "<A>", "cast_at": "2026-10-17T12:00:12Z", "already_completed": false, "damage": 274, "dealt": 274, "boss": 3, "defeated_boss": true}
]
```
- Newest first; `[]` before the first cast, or for a team in the lobby.
- Read from memory, like every other read. Not pushed on the stream: `team_state.last_cast` carries each new cast, so a client loads the log once and prepends each new `last_cast` it sees (by `cast_at`), which is what `/play` does.

**`Game`** (`GET /api/game/shapes`), with the 2026 seed and config.toml as shipped:
```json
{
  "shapes": [
    {"id": "hexagon",       "name": "Hexagon",       "patterns": 210, "points": [6], "damage": {"min": 1200, "max": 1200}, "patterns_by_points": {"6": 210}, "classes": ["class_6"]},
    {"id": "line",          "name": "Line",          "patterns": 45,  "points": [2], "damage": {"min": 25, "max": 36},     "patterns_by_points": {"2": 45},  "classes": ["class_2"]},
    {"id": "pentagon",      "name": "Pentagon",      "patterns": 252, "points": [5], "damage": {"min": 395, "max": 403},   "patterns_by_points": {"5": 252}, "classes": ["class_5"]},
    {"id": "quadrilateral", "name": "Quadrilateral", "patterns": 210, "points": [4], "damage": {"min": 195, "max": 278},   "patterns_by_points": {"4": 210}, "classes": ["class_4"]},
    {"id": "triangle",      "name": "Triangle",      "patterns": 120, "points": [3], "damage": {"min": 64, "max": 75},     "patterns_by_points": {"3": 120}, "classes": ["class_3"]}
  ],
  "health": 10,
  "unlocks": [{"after_bosses": 0, "points": 3}, {"after_bosses": 2, "points": 4}, {"after_bosses": 4, "points": 5}, {"after_bosses": 5, "points": 6}],
  "max_pattern_points": 6,
  "spell_classes": [{"id": "class_1", "name": "Class 1", "points": 1}, {"id": "class_2", "name": "Class 2", "points": 2}, …, {"id": "class_15", "name": "Class 15", "points": 15}]
}
```
- `shapes`: every selectable shape (the client's "types"), in index order (by ID). `id` is what `PUT …/selection` takes; `name` is for display; `patterns` is how many valid patterns the shape has; `points` is the distinct sizes of those patterns, ascending (a shape mixing 3- and 4-point patterns shows `[3, 4]`); `damage` is the minimum and maximum damage across the shape's patterns; `patterns_by_points` counts its patterns by size (JSON keys are the sizes): a team can cast those of at most its `team_state.unlocked_points`.
- `health`: **10** (`game.health`), every team's starting health and what a revive restores.
- `unlocks`: the unlock schedule (`game.unlocks`), ascending by `after_bosses`: once a team has defeated `after_bosses` bosses, it can cast patterns of up to `points` waypoints (3 from the start, 4 after boss 2, 5 after boss 4, 6 after boss 5). Where a given team stands is in its `team_state` (`unlocked_points`, `locked_shapes`), not here: this route is the same for everyone.
- `max_pattern_points`: **6** (`game.max_pattern_points`).
- Any signed-in user may read it, in a team or not. It's built once at startup and never changes while the backend runs; no live event carries it.

**`ScanResult`:**
```json
{"outcome": "pattern_completed",
 "pattern": ["2001", "2002", "2007", "2008"],
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
| `ErrWaypointOnCooldown` (`Scan`) | 409 | `waypoint_on_cooldown` |
| `ErrSelectionPoisoned` | 409 | `selection_poisoned` |
| `ErrKnockedOut` | 409 | `knocked_out` |
| `ErrNotLeader` (`Select`, `ClearSelection`) | 403 | `not_leader` |
| `ErrShapeLocked` (`Select`) | 409 | `shape_locked` |
| `ErrShapeOnCooldown` (`Select`) | 409 | `shape_on_cooldown` |
| `ErrTeamNotFound` (`Revive`) | 404 | `team_not_found` |
| `ErrInvalidQR` | 400 | `invalid_qr` |
| `ErrExpiredQR` | 409 | `qr_expired` |
| `ErrUnknownPowerup` | 404 | `unknown_powerup` |
| `ErrPowerupNotHeld` | 409 | `powerup_not_held` |
| `ErrPowerupActive` | 409 | `powerup_active` |
| `ErrFullHealth` | 409 | `full_health` |
| `ErrUnknownPattern` | 404 | `unknown_pattern` |
| `ErrPatternLocked` | 409 | `pattern_locked` |
| `ErrUnknownQTE` (qte's own) | 404 | `unknown_qte` |
| `ErrInvalidOption` | 400 | `invalid_option` |
| `ErrQTEAlreadyAnswered` | 409 | `qte_already_answered` |
| `ErrQTEClosed` (qte's own) | 409 | `qte_closed` |
| a `{qte_id}` that isn't an integer | 400 | `invalid_qte_id` |
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
| `GrantPowerup` (an admin) grants one the team didn't hold | `powerup_granted` | members |
| `Activate`: a damage_multiplier, heart or short_qr_cooldown | `powerup_activated` | members |
| `InstantCast` | `spell_cast` or `boss_defeated` | members |
| `AnswerQTE` | `qte_answered` | members |
| a short_qr_cooldown window ends (`catchUp`), with **nobody acting**, and no attack fell due at the same moment | `powerup_ended` | members |
| a QR cooldown ends (`catchUp`, from the timer or the start of a write), with **nobody acting**, and no attack fell due at the same moment | `qr_cooldown_ended` | members |
| an admin's `Revive` | `revived` | members |
| an admin's `ResetCooldowns` | `cooldowns_reset` | members |
| a cast that defeats the last boss | `boss_defeated` as usual; then teams' `team` of type `finished` (`Rules.OnFinished`) | members |
| an admin's deletion (`TeamDeleted`) | nothing here; teams sends `no_team` (`team_deleted`) | |
| `reconcile` | `resync` | members |

Never after a rejection or no-op. Every change bumps `version.seq` by one; `snapshot` and `game_started` repeat the current version. The responses (`GET` state, both selection routes, `ScanResult.state`) carry the same `version`, so a client keeps whichever copy is newest.

The only events published with nobody acting are boss attacks (`boss_attacked`, `knocked_out`), QR cooldowns ending (`qr_cooldown_ended`) and short_qr_cooldown windows ending (`powerup_ended`), from the team's timer. Every trigger, the ordering guarantees and the wire format: [`internal/server/README.md`, "The event stream"](../server/README.md#the-event-stream).

## Rules and cases

### Loading at startup
The backend refuses to start if any of these hold:

| Case | Error |
|---|---|
| `config.toml` is invalid (see `internal/platform/config`), e.g. a missing key, `max_pattern_points` outside 1–15, or a `game.unlocks` with no `0` entry, a key outside 0–`game.bosses`, a value outside 1–`max_pattern_points`, a step that doesn't raise the points, or a last step short of `max_pattern_points` | from `config.Load`, before anything loads |
| The content is invalid (bad waypoint IDs, too many items, unknown, empty, oversized, duplicate or nested patterns, missing damage) | from `atlas.Load`, before this `Load` runs: every case, in order, is in [`internal/atlas/README.md`](../atlas/README.md), "Loading at startup" |
| Reading the per-team tables fails | `load team state: …` |
| Stored visited waypoints name a waypoint the atlas doesn't have | `team 482913 state: unknown waypoint "2011"` |
| A `team_pattern` key the atlas doesn't have | `team 482913 state: unknown pattern 400` |
| A selection's shape the atlas doesn't have | `team 482913 state: selection: unknown shape "square"` |
| A `team_cast` row's pattern the atlas doesn't have | `team 482913 state: cast: unknown pattern 400` |
| A stack scan's waypoint the atlas doesn't have | `team 482913 state: stack scan: unknown waypoint "2011"` |
| The boss content is invalid (wrong count for `game.bosses`, gaps, no name or asset, HP ≤ 0, attack interval ≤ 0, attack damage < 0) | from `bosses.Load`, before this `Load`: [`internal/bosses/README.md`](../bosses/README.md), "Loading at startup" (an empty `boss` table is allowed) |
| A `team_boss` row with more bosses defeated than `game.bosses` | `team 482913 state: team_boss: 7 bosses defeated, but game.bosses is 6` |

Not a refusal: a team still `IN_GAME` with every boss defeated (the backend stopped between the cast and the mark) is finished at the end of `Load` (`Rules.OnFinished`).

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
| Every pattern of the shape has more waypoints than the team's `unlocked_points` (with the 2026 schedule: quadrilaterals before boss 2, pentagons before boss 4, hexagons before boss 5) | `409 shape_locked`: nothing written, no event |
| Every unlocked pattern of the shape is of the team's cooldown class (its last cast's; with the 2026 seed, whose shapes are each one size, the shape of the last cast) | `409 shape_on_cooldown`: nothing written, no event |
| A shape mixing the cooldown class and another | Allowed: only its patterns of other classes can be cast (see Scan) |
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
| (any boss attack or QR cooldown end that fell due is applied first: `boss_attacked` / `knocked_out` / `qr_cooldown_ended`) | |
| Team knocked out | `409 knocked_out` |
| No shape selected (including right after a completion) | `409 no_shape_selected` |
| The stack holds a wrong scan (poisoned), whatever the waypoint, whoever scans | `409 selection_poisoned` |
| Waypoint already in the stack, by anyone (a retry included) | `409 waypoint_already_scanned` |
| Waypoint in a cast of the team's less than **5 min** ago (`game.qr_cooldown`), by any member | `409 waypoint_on_cooldown`: nothing pushed, logged or published; the attempt is untouched |
| ... 1 µs before its `until` | `409 waypoint_on_cooldown` |
| ... exactly at its `until` | Allowed (the expiry is applied first: `qr_cooldown_ended`) |
| A waypoint another team cast | Allowed: each team has its own cooldowns |
| Caller already has scans in the stack | Allowed: any member may scan any number of waypoints |
| Any time since the selection or the last scan, however long | Allowed: there's no time limit |
| The waypoint, with the stack, isn't part of any pattern of the selected shape **of at most `unlocked_points` waypoints and not of the cooldown class** (a set only a locked or cooled-down pattern contains is wrong) | `200 wrong_scan`: pushed with `wrong: true`, `poisoned: true`; logged with `wrong = true`; event |
| Stack plus this scan is exactly a pattern of the **selected** shape | `200 pattern_completed`: pattern and shape marked; **stack and selection purged**; cast appended to the log and `last_cast` (`already_completed: false`); `team_cast` +1; the damage × multiplier dealt to the current boss, `team_boss` upserted; event `spell_cast` |
| ... and the damage brings the boss to its HP | The boss is defeated: `bosses_defeated` + 1, `boss` the next one at 0 damage (or `null` after the last); the excess is lost; `defeated_boss: true`; the next boss's phase starts now with the current health (`team_boss.phase_*`), timer re-armed; event `boss_defeated` |
| ... and every boss is already defeated (or none are loaded) | Cast as usual with `dealt: 0`, `boss: null`; progress unchanged; event `spell_cast` |
| ... that pattern was already completed | `pattern_completed` with `already_completed: true`; progress unchanged; cast appended (`already_completed: true`); `team_cast` +1, `team_pattern` unchanged |
| Stack plus this scan is a pattern of **another** shape | Not completed → `scan_added` |
| Otherwise | `scan_added` |
| 1-point pattern | Completes on its only scan |
| 6-point pattern, once unlocked (after boss 5) | Completes on the sixth scan (the stack never needs its seventh slot for correct scans) |
| A pattern bigger than `unlocked_points` (in a shape mixing sizes) | Can't complete: the scan that would leave the unlocked patterns is wrong |
| A pattern of the cooldown class (the last cast's) | Can't complete: the scan that leaves every pattern of other classes is wrong. After a cast of another class it can again |
| A cast | Its pattern's class becomes the cooldown (`cooldown_class`), and its waypoints go in `qr_cooldowns` until now + `game.qr_cooldown`, in the same event |
| A non-leader scanning | Allowed: every member scans |
| Any accepted scan (wrong included) | Logged in `team_scan` (with `wrong`); waypoint added to visited `waypoints` |
| Postgres write fails | `500`; state reloaded from Postgres; nothing applied (a failed cast starts no QR cooldown) |

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
| Team in the lobby or with no activity | `200`, empty lists, `selection` and `last_cast` null, `poisoned: false`, `bosses_defeated: 0`, `boss: {serial: 1, damage: 0, …}` (or `null` with no bosses loaded), `damage_multiplier: 1`, `health: 10` less any attacks since the start, `knocked_out: false`, `unlocked_points: 3`, `locked_shapes` (`["hexagon", "pentagon", "quadrilateral"]` with the 2026 seed), `qr_cooldowns: []` |
| Knocked out | `200`, `health: 0`, `knocked_out: true`, `boss.next_attack_at: null` |
| Reads never catch up | Health is as of the last `catchUp`; the timer keeps it current, so a read can only lag a due attack by the timer's delay |
| Poisoned attempt | `200`, the stack with its wrong scan marked, `poisoned: true` |

### Time-driven changes: the timer, and `catchUp`
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
| A QR cooldown ends (its `until` reached) | Dropped from `qr_cooldowns`, `qr_cooldown_ended`; nothing written. Several ending at once are one change |
| A QR cooldown ends at the same moment as an attack | One change, one event: `boss_attacked` (or `knocked_out`) |
| Knocked out, with QR cooldowns | No attacks, but the timer still runs for the cooldowns: `qr_cooldown_ended` |
| `game.qr_cooldown` 0 | No QR cooldowns; the timer follows attacks only |
| A short_qr_cooldown window ends | `powerups.active` loses it, `powerup_ended`; nothing written |
| A window ends at the same moment as an attack | One event: `boss_attacked` (or `knocked_out`) |

### Revive: `Revive`, behind `POST /api/admin/teams/{team_id}/revive`
The admin use case checks first (`401`, `403 not_admin`, `400 invalid_team_id` for an ID that isn't 6 digits); then, in this order:

| Case | Result |
|---|---|
| No team with that ID | `404 team_not_found` |
| Team not `IN_GAME` | `409 not_in_game` |
| (any boss attack or QR cooldown end that fell due is applied first) | |
| Knocked out | `200`: health **10** (`game.health`), a new phase of the current boss from now, `attacks: 0`, timer re-armed; `team_boss` upserted; event `revived` |
| Not knocked out | The same: it also heals, and restarts the boss's clock. QR cooldowns are kept |
| Every boss defeated | Health restored; nothing attacks |
| Postgres write fails | `500`; state reloaded; nothing applied |

### Reset cooldowns: `ResetCooldowns`, behind `POST /api/admin/teams/{team_id}/cooldowns/reset`
The admin use case checks first (`401`, `403 not_admin`, `400 invalid_team_id`); then, in this order:

| Case | Result |
|---|---|
| No team with that ID | `404 team_not_found` |
| Team not `IN_GAME` (lobby, or `FINISHED`) | `409 not_in_game` |
| (any boss attack or QR cooldown end that fell due is applied first) | |
| Otherwise | `200` with the state: `cooldown_class` null, `cooldown_shapes` and `qr_cooldowns` empty; `team_boss.cooldowns_reset_casts` upserted to the log's length; event `cooldowns_reset`; timer re-armed. Health, bosses and the selection are untouched |
| Nothing was cooling | The same (a harmless write) |
| The next cast, even in the same instant | Starts its class and QR cooldowns as usual (it's after the reset's position in the log) |
| A restart | The reset stays: casts before the stored position still don't count |
| Postgres write fails | `500`; state reloaded; nothing applied |

### Finish: `Rules.OnFinished`, no request
| Case | Result |
|---|---|
| A cast defeats the last boss | After `Scan` (or `InstantCast`) releases the lock, `OnFinished(teamID)` (teams' `Finish`): the team is `FINISHED`; the cast's own response and events are as usual |
| That cast's write fails | Not finished: `500`, the reload still has the last boss alive, and the team plays on; unless the write committed before failing (a timeout), when the reload has every boss defeated and the team is finished then |
| Any write after | `409 not_in_game` (`inGameTeam`: not `IN_GAME`) |
| `Load` finds a team `IN_GAME` with every boss defeated | `OnFinished` again (the mark never happened) |
| No bosses loaded | Never called (no team can defeat "every" boss) |

### Powerups: `POST /api/teams/me/state/powerups/{kind}`, and `GrantPowerup`
| Case | Result |
|---|---|
| `{kind}` unknown | `404 unknown_powerup` |
| `instant_cast` body missing, empty, or an ID twice | `400 invalid_body` |
| `instant_cast`: an unknown waypoint, or waypoints making no pattern | `404 unknown_waypoint`, `404 unknown_pattern` |
| Caller not in a team / team not `IN_GAME` (FINISHED included) | `404 not_in_team` / `409 not_in_game` |
| (any due attack or expiry is applied first) | |
| Not the leader | `403 not_leader` |
| Knocked out | `409 knocked_out` (a heart too: knocked out still needs an admin) |
| Not held (never granted, or used: a resend lands here) | `409 powerup_not_held` |
| A `damage_multiplier` while one is armed; a `short_qr_cooldown` while its window runs | `409 powerup_active` (the held one is kept) |
| A `heart` at full health | `409 full_health` |
| An `instant_cast` of a pattern bigger than `unlocked_points` | `409 pattern_locked` |
| An `instant_cast` of the cooldown class, or of cooling waypoints | Allowed: it bypasses both |
| Otherwise | `200`, the kind leaves `held`, its effect applies, `powerup_activated` (an instant cast: its cast's event) |
| `GrantPowerup`: unknown kind / team / not `IN_GAME` | `ErrUnknownPowerup` / `ErrTeamNotFound` / `ErrNotInGame` |
| `GrantPowerup` of a kind already held | `granted: false`: wasted, nothing written or published |
| `GrantPowerup` to a knocked-out team | Granted |
| Persist fails | `500`; state reloaded; nothing applied |

### Quiz answers: `POST /api/teams/me/state/qte/{qte_id}/answer`
| Case | Result |
|---|---|
| `{qte_id}` not an integer; body without `option`, or negative | `400 invalid_qte_id`; `400 invalid_body` |
| No such QTE, or a draft (players never see drafts) | `404 unknown_qte` |
| `option` past the last | `400 invalid_option` |
| Caller not in a team / team not `IN_GAME` (FINISHED included) | `404 not_in_team` / `409 not_in_game` |
| (any due attack or expiry is applied first) | |
| The team has answered it (a teammate, or a resend; checked before the close, so a resend after it still lands here) | `409 qte_already_answered` |
| Now at or after its `ends_at` (time ran out, or an admin ended it), in memory or in Postgres (an `End` committed but not yet applied) | `409 qte_closed` |
| Knocked out | `409 knocked_out` |
| Otherwise | `200`, `qte` set: `correct`, and `granted` with the powerup added to `held` unless already held; `qte_answered` |
| Two members at once | One is recorded (the lock, and the primary key behind it); the other is `qte_already_answered` |
| Persist fails | `500`; state reloaded; nothing applied |

### Delete: `TeamDeleted`, no request (teams' `Rules.OnDelete`)
| Case | Result |
|---|---|
| The team has an entry | Removed from the index; its timer stopped; the entry closed. Nothing published |
| No entry | No-op |
| A write already holding the entry | Finishes first; its persist hits the deleted team and reconciles (to nothing) |
| A write after | `404 not_in_team` (`inGameTeam`: teams no longer knows the user's team) |
| The timer firing after | Finds no entry (or a closed one) and does nothing (`due` never creates an entry) |

## Concurrency and locking
- **Writes to one team are serialised** on its `entry.mu`. Different teams run in parallel.
- **Two members scan the same waypoint at once:** exactly one is pushed; the other gets `waypoint_already_scanned`.
- **One member scans two waypoints at once:** both are pushed, in lock order (if both fit).
- **A phone resends one scan many times at once:** exactly one is pushed and logged; the rest get `waypoint_already_scanned`.
- **A wrong scan races a correct one:** whichever takes the lock first is pushed first. If the correct one wins, the wrong one is then checked against a bigger stack (still wrong: `FitsShapeCastable` only gets stricter as the set grows) and poisons. If the wrong one wins, the correct one gets `selection_poisoned`. Either way every phone ends poisoned, with the same stack (`TestSimWrongScanReachesEveryone`).
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
- **The unlock versus a select or scan.** `unlocked_points` is derived from `State.Boss.Defeated` in the same snapshot the write validates against, under the entry lock: a scan racing the cast that crosses an unlock step (defeats boss 2, 4 or 5) runs either before it (the old limit) or after it (the new one), never half.

  So no write by a removed player is accepted once the removal is visible, and none is half-applied. Their earlier scans stay in the stack and still count toward a pattern (`TestAdminRemoveFromStartedTeam`, `TestAdminRemoveRacesScan`). After `RosterChanged`, publishes go to the remaining members only. Lock order is the same as the start hook's: teams entry → teamstate entry.
- **The finish hook runs outside the lock.** `Scan` calls `Rules.OnFinished` from a deferred call that runs after the deferred unlock, so teams' `Finish` takes teams' entry lock while team state holds nothing. The cross-use-case order stays teams entry → teamstate entry (the start, roster and delete hooks take them that way); holding teamstate's lock while waiting for teams' would be the reverse, a deadlock (`TestFinishedHook` proves the hook runs unlocked by taking the team's lock inside it). Between the cast and `Finish` another write can slip in and is accepted (it deals nothing: every boss is defeated); after `Finish`, writes are `not_in_game`.
- **A deletion versus a write or the timer.** `TeamDeleted` runs under teams' entry lock, after teams has removed the team. It takes the index lock to drop the entry, then the entry's own lock to stop the timer and close it, never both at once (`Close` takes the index lock, then each entry's: holding them the other way round could deadlock). A write already holding the entry finishes first (its persist fails on the missing team and reconciles); a later one is `not_in_team`; `due` only looks entries up, so a timer firing late can't recreate the team.
- **A cooldown reset versus a cast.** Both take `entry.mu`. The reset stores the log's length, so a cast after it in lock order counts for cooldowns even at the same timestamp.
- **The team's timer versus writes.** The timer's callback (`due`) takes `entry.mu` like a write, so an attack or expiry and a write never interleave. Whichever takes the lock first applies what's due (`catchUp`); the other finds nothing more due. So every attack and every QR cooldown end is applied exactly once, before any write that comes after its time, and its event precedes that write's. A timer delayed behind a held lock costs nothing: the write catches up.
- **A cast versus a due attack.** If an attack fell due before a cast but hadn't been applied, the cast's `catchUp` applies it first, so the cast's `deal` sees the real health and a defeat starts the next phase with it.
- **Two members scan a cooling waypoint, or one scans it as the cooldown ends.** The check runs under `entry.mu` against the snapshot after `catchUp`, so a scan at or after `until` always sees it ended, and one before always sees it cooling.
- **Timers and restarts.** Timers live in memory only. `Close` stops them (on shutdown, and in tests before a second store loads); `Load` re-arms every started team. Because attacks and expiries write nothing, even a stray timer of an old store could only change that store's memory, never Postgres.
- **Two members answer a quiz at once:** both take `entry.mu`; the first is recorded, the second finds it in `QTEAnswers` (`qte_already_answered`). `team_qte_answer`'s primary key backs it.
- **An answer racing an admin's `End`.** `End` takes qte's lock, not this one, and writes Postgres before memory, so for up to `storage.write_timeout` an answer can see the QTE still open in memory after `End` committed. The insert checks the stored `ends_at` too (`answered_at < ends_at`): such an answer is `409 qte_closed`, with nothing stored or granted. The stored `ends_at` is final; a late close timer never lets an answer in either.
- **The memory checks are the only enforcement** of the stack rules (one scan per waypoint per attempt, nothing after a wrong scan): they depend on the selection's `after_scan_id` window, which a unique constraint can't express. They all run under `entry.mu`, and Postgres has one writer (the instance lock in `platform/postgres`).

## Failure and recovery
| Case | Result |
|---|---|
| Postgres times out or drops during a write | `500`. `reconcile` re-reads the team (the write may or may not have committed) and memory matches Postgres. A final cast finishes the team only if the reload shows it committed |
| Memory and Postgres disagree (rows edited by hand) | The next write that notices (e.g. `deleteSelection` finding no row) fails, reconciles, and members get the corrected state |
| Postgres unreachable | Reads keep working from memory (the cast log included); writes fail |
| Backend restarts | Progress, selections, stacks (with their `wrong` flags, so `poisoned` too), the cast log and `last_cast`, the boss progress, multiplier and phase reload exactly. Health is recomputed from the phase and the clock: **attacks that fell due while the backend was down are applied at load**, as if it had stayed up (a team can come back knocked out). QR cooldowns are rebuilt from the cast log, without those that ended while down. Timers are re-armed |
| `game.boss_attack.interval` or a boss's damage changed in `config.toml` between restarts | Applies retroactively to every current phase against that boss: health jumps to what the new values give from the phase start |
| `game.qr_cooldown` changed between restarts | Applies retroactively: every recent cast's waypoints cool until its `cast_at` + the new value (raising it can bring back ones that had ended; 0 clears them all) |
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
| The unlock schedule: 3 points from the start, 4 after boss 2, 5 after boss 4, 6 after boss 5 | `config.toml` `[game.unlocks]` | Edit and restart. `0 = 6` (`max_pattern_points`) alone locks nothing. A restart re-derives each team's unlock from `bosses_defeated`, so nothing is stored |
| Cast log length | none | Unbounded; 80 bytes per cast in memory, and a cast copies the team's log once |
| Starting health **10** | `config.toml` `game.health` (at least 1) | Edit and restart |
| Attack interval **10 min** | `config.toml` `game.boss_attack.interval` (positive; whole seconds, checked by `bosses.New`) | Edit, restart; applies retroactively (above) |
| Attack damage, by boss (**= serial**, 1…6) | `config.toml` `[game.boss_attack.damage]` (exactly serials 1…`game.bosses`, each ≥ 0) | Edit, restart; applies retroactively |
| QR cooldown **5 min** | `config.toml` `game.qr_cooldown` (≥ 0; 0 turns it off) | Edit, restart; applies retroactively (above) |
| Powerups: ×**2**, +**1** health, **2 min** for **5 min** | `config.toml` `[powerups]` (`damage_multiplier` 1–100, `heart` ≥ 1, `short_qr_cooldown` 0–`game.qr_cooldown`, `short_qr_window` > 0) | Edit, restart. `short_qr_cooldown` applies retroactively; a window running at a restart keeps its start, with the new length |
| One pending timer per started team (attacks and QR cooldowns share it) | `time.AfterFunc` | ~100 bytes and no goroutine while waiting; 1,666 teams at 10,000 players is negligible |
| 64 waypoints, 64 shapes, 2048 patterns | the atlas's set types (`atlas.MaxWaypoints`, `atlas.MaxShapes`, `atlas.MaxPatterns`) | See [`internal/atlas/README.md`](../atlas/README.md), "Limits and how to change them". `State` holds one of each set, so a wider set makes every snapshot bigger |

## Not decided yet
- **⚠ The frontend play page doesn't scan QR yet.** The backend verifies the token (`POST …/scans` takes `{"qr"}`); the play page still submits a raw ID, so it gets `400 invalid_body` until it's wired to the camera scanner (`/test-scan` already reads tokens).
- **No wrong scans with the 2026 content.** Every combination of the 10 checkpoints is a pattern, so a scan can't break an attempt: `wrong_scan`, `poisoned` and `selection_poisoned` only happen with a waypoint in no pattern (the tests add `2099`). Whether the real event should have a decoy waypoint, or shapes that don't hold every combination, is open.
- **A non-leader can't restart a poisoned attempt.** Only the leader selects, so a team whose leader is away can't purge a wrong scan until the leader acts (or an admin removes the leader and names another).
- **Winning** is the leaderboard's ([`internal/leaderboard/README.md`](../leaderboard/README.md)): bosses defeated, then speed. A team through the last boss is `FINISHED` and plays no more; nothing declares one winner.
- **What sets the multiplier.** Stored and applied, never changed by any route yet.
- **Boss content changes mid-event apply retroactively.** Health is derived from the phase start with the boss's *current* interval and damage; storing them in the phase would freeze them.
- **What being knocked out costs.** Only the wait for an admin: progress, patterns and the cast log are kept, and a revive restores full health (and restarts the current boss's clock). No automatic revive, no penalty.
- **Clients see attacks no earlier than the server applies them.** `next_attack_at` is a countdown; the attack itself arrives as `boss_attacked` (at most the timer's delay later).
- **Content with one castable class would trap a team.** The cooldown assumes a team always has another class to cast: true with the 2026 seed (lines and triangles from the start, a class more at each unlock step, five classes after boss 5). Content whose only unlocked patterns are of a single class would leave a team, after its first cast, with nothing it can cast. Nothing checks for that at startup.
- **Cooldown resets are by hand.** An admin's `ResetCooldowns` clears both cooldowns (the escape hatch for a team stuck on one class or a cooled map); nothing resets them automatically, and a revive doesn't.
- **Small maps and the QR cooldown.** With the 2026 seed's 10 waypoints, a quadrilateral cools 4 of them and a hexagon 6, and a pentagon or hexagon needs 5–6 waypoints free at once. The game-loop simulation uses a 2-minute cooldown for this reason; the real map's size decides whether 5 minutes is right.

- **The frontend doesn't show powerups yet.** `team_state.powerups` and the casts' `instant` / `boosted` / `short_qr_cooldown` flags aren't in `frontend/src/lib/types/Game.type.ts` (`TeamStateSchema`), and `/play` has no inventory or use buttons (AGENTS.md §17, item 7). Nor is `team_state.qte`, and there's no quiz screen for the `qte` event.

## Tests
- The content lookups teamstate relies on (`PatternFor`, `FitsShapeCastable`, `SetOf`, validation, the seed loading, `BenchmarkLookups`) are tested in the atlas: [`internal/atlas/README.md`](../atlas/README.md), "Tests".
- `tests/stack_test.go` (no database):

  | Test | Covers |
  |---|---|
  | `TestStack` | `NewStack(3)` has `Cap` 4 (a pattern's points plus one wrong scan); bottom-to-top order; a wrong scan on top keeps its flag; `Push` leaves the original alone; a push onto a full stack panics |

- `tests/store_test.go` (real Postgres, schema `teamstate_test`, the 2026 seed loaded through `atlas.Load` and `bosses.Load`, plus waypoint `nowhere` (`2099`, in no pattern) for wrong scans, config.toml's rules including `game.health`, a fake clock). The fake `clock` implements `teamstate.Clock`: `Now` returns the fake time; `AfterFunc` records a timer; `at(d)` sets the time to `start` + `d` and then runs every timer now due, earliest first (never from inside `AfterFunc`, since the store calls it holding the team's lock); `pending()` counts live timers. Every helper (`scanAt`, `selectAt`) sets the clock first, so stored times are predictable. Times are written with `tick` (seconds). The fixture pins the team's `started_at` to `start` (and reloads teams), so boss 1's attacks fall at exactly `start` + k × interval. **Bosses don't attack unless a test asks** (`fixtureOptions.attacks`): otherwise `bossRules(set, attacks)` builds the fixture's `bosses.Rules` from `config.toml` (`game.bosses`, `[game.boss_attack]`) with the interval set to `neverAttack` (10 years), so tests that move the clock by minutes see no attacks. `fixtureOptions.bossRules` adjusts those rules for one test (an interval or a boss's damage). **Every pattern is unlocked unless a test asks** (`fixtureOptions.unlocks` keeps `config.toml`'s `game.unlocks`; otherwise `Rules.Unlocks` = `{0: MaxPatternPoints}`), so tests about other things can cast any shape. **The QR cooldown is off unless a test asks** (`fixtureOptions.qrCooldown` keeps `game.qr_cooldown`), since most tests cast the quadrilateral and the triangle (sharing 2001 and 2002) seconds apart. The tests use up to 6 members (`minTestTeam`), and the seed's 6 bosses (`seedBosses`); with a smaller `teams.size`, or another `game.bosses`, in `config.toml` they **skip** rather than fail:

  | Test | Covers |
  |---|---|
  | `TestWritesRequireGame` | input validation and lobby refusals for every write |
  | `TestScanNeedsSelection` | scanning before selecting changes nothing |
  | `TestQuadCasts` | walkthrough 1: a waypoint rejection changes nothing, one member scans two waypoints, the line and triangle that don't count, completion, `last_cast`, no selection after |
  | `TestOneMemberScansWholePattern` | walkthrough 5: one member scans every point of the quadrilateral |
  | `TestNoTimeLimit` | walkthrough 3: scans hours apart stay in the stack and still complete |
  | `TestEverySelectionPurges` | walkthrough 4; reselect and select-another both purge |
  | `TestRescanIsRejected` | the same member's retry and another member's scan of a stacked waypoint: rejected, not logged, no event |
  | `TestConcurrentRescansAddOneScan` | 8 simultaneous copies → one pushed, seven `waypoint_already_scanned` |
  | `TestWrongScanIsReported` | a waypoint in no pattern (`2099`): `wrong_scan`, marked wrong in the stack, `poisoned`, logged; later scans by the scanner and a teammate get `selection_poisoned` |
  | `TestWrongScanPoisonsSelection` | walkthrough 2: a poisoned attempt refuses the rest of the quadrilateral; a reselect clears it and the quadrilateral then completes |
  | `TestCompletionPurges` | stack and selection purged; rescans need a new selection (the triangle: the quadrilateral is on cooldown after its cast) |
  | `TestOutOfOrderScanning` | a quadrilateral completes when scanned in reverse order (a pattern is an unordered set) |
  | `TestSinglePointPattern` | a 1-point pattern (shape `dot` on `2010`, added by SQL) |
  | `TestSixPointPatternAtCapacity` | a seed hexagon (`game.max_pattern_points`) completes on the sixth scan (the fixture's default: every pattern unlocked from the start) |
  | `TestUnlockSchedule` | config.toml's schedule (`unlocks: true`), every boss at 1 HP, a shape `mixed` added by SQL (a 4-point pattern and a 5-point one, through waypoints `2099` and `2098`): it walks the schedule casting only triangles and lines (`castSmall`), and before each cast checks `unlocked_points` is the schedule's value for the bosses defeated, `locked_shapes` is exactly the shapes whose patterns are all bigger, and selecting each is `ErrShapeLocked`; at the 4-point step (`bossesFor(rules, 4)`) `mixed` selects and a scan of `2098`, only in its 5-point pattern, is a wrong scan; at the 5-point step a seed pentagon casts. Skips for a schedule that doesn't open with at most triangles, then quadrilaterals, then pentagons |
  | `TestOnlyLeaderSelects` | a member's select and clear are `ErrNotLeader` and change nothing; the leader's select works and members still scan |
  | `TestCompletingAgain` | the quadrilateral, a triangle (the cooldown forbids two quadrilaterals in a row), the quadrilateral again: `already_completed` on the repeat, progress counted once |
  | `TestSpellCooldown` (`tests/cooldown_test.go`) | the spell cooldown, with a shape `mixed` added by SQL (a 3-point and a 4-point pattern through a new waypoint `2097`): no cooldown before a cast; after the quadrilateral, `cooldown_class` `class_4` and `cooldown_shapes` `quadrilateral`; selecting the quadrilateral is `shape_on_cooldown` with nothing written or published; `mixed` selects, but a point only in its 4-point pattern is a wrong scan; its 3-point pattern casts, and the cooldown becomes `class_3` (`triangle`); a restart rebuilds it from the cast log; the quadrilateral casts again after that; boss 1's defeat by the first cast didn't clear it |
  | `TestCooldownFailedCastKeepsIt` | with the database closed, the triangle that would clear the quadrilateral's cooldown fails: the cooldown, the last cast and `cast_count` stay the quadrilateral's |
  | `TestCooldownKnockedOutFirst` | every boss hitting for 100: knocked out by boss 2 with the quadrilateral on cooldown, selecting the quadrilateral (or the triangle) is `knocked_out`, not `shape_on_cooldown` |
  | `TestCooldownOnBigPatterns` | config.toml's schedule (`unlocks: true`, every boss at 1 HP): once pentagons are unlocked (bosses defeated by triangles and lines, `castSmall`), casting a seed pentagon puts `class_5` on cooldown: the pentagon moves to `cooldown_shapes`, not `locked_shapes`, and is `shape_on_cooldown`; a quadrilateral opens it again |
  | `TestCooldownAfterEveryBoss` | a cast after every boss is defeated deals nothing but still sets the cooldown |
  | `TestClearSelection` | clear with and without a selection; waypoints kept |
  | `TestRestartRestoresEverything` | restart reproduces the JSON view exactly, except `version`, which restarts at seq 0 in the new epoch; rescans still rejected |
  | `TestConcurrentScans` | two members at one waypoint (one accepted); one member at two waypoints (both accepted) |
  | `TestFailedWriteReconciles` | reload after a hand-edited database, published as `resync` one version on |
  | `TestVersionAndTypes` | (boss 1 given huge HP, so the cast stays a plain `spell_cast`) each publish's type (`game_started`, `shape_selected`, `scan_added`, `spell_cast`, `wrong_scan`, `selection_cleared`) and version: `PublishTeam` repeats it, every change adds one, a rejected scan, a no-op clear and a selection refused for the cooldown publish nothing and keep it; `cast_count` 0 until the cast, then 1; `Snapshot` is of type `snapshot` |
  | `TestReadsDoNotTouchDatabase` | reads with the database closed; a failed write leaves memory alone |
  | `TestStateIsPublished` | live events reach teammates |
  | `TestPublishTeam` | `PublishTeam` sends the empty state to every member of a started team; after a selection it sends the current state (quadrilateral selected) without changing it; nothing for an unknown team or a lobby team |
  | `TestCastLog` | walkthrough 6: no casts before; `last_cast` (pattern, shape, `class`, `cast_by`, `cast_at`, `damage`) after each of three casts (quadrilateral, triangle, quadrilateral: the cooldown forbids two quadrilaterals in a row), the repeat marked `already_completed`; a new selection keeps them; `CastsForUser` newest first with their classes; three `team_cast` rows; a restart reloads the same log (JSON equal) |

- `tests/boss_test.go` (same fixture; `castQuad` selects and scans the seed quadrilateral, `restart` closes the store and loads a new one from the same database):

  | Test | Covers |
  |---|---|
  | `TestCastsDamageBosses` | walkthrough 7 against a model built from the seed's HPs: casts the quadrilateral and the triangle in turn (the cooldown) until two bosses fall, checking after each `dealt`, `boss`, `defeated_boss`, `boss.serial`/`damage`, `bosses_defeated`, the event type (`spell_cast`, then `boss_defeated` on a defeat), the excess lost; `team_boss` in Postgres; a restart reproduces the view (except `version`) and the cast log |
  | `TestDamageMultiplier` | `team_boss.multiplier = 1.5` set in Postgres, restart: `damage_multiplier` 1.5; a cast's `dealt` is `round(damage × 1.5)` while `damage` is the pattern's; the multiplier survives the cast's upsert and another restart |
  | `TestDealtClamped` | a stored multiplier of 1e12: the cast writes, dealing `math.MaxInt32` and defeating the boss; a restart reproduces it |
  | `TestEveryBossDefeated` | every boss at 1 HP: each cast (quadrilateral and triangle in turn) defeats one; after the last, `boss` null and `bosses_defeated` = the count; one more cast counts (`cast_count`) but deals 0 and hits no boss |
  | `TestLoadRefusesTooManyDefeated` | a `team_boss` row with `defeated` above `game.bosses` stops `Load` |
  | `TestPersistFailures` (`failure_test.go`) | every write-through failing in Postgres at each step (a trigger raising on one table): select (nothing stored yet, so the reload is the empty state), clear, a cast's `team_scan`, `team_pattern`, `team_cast` and `team_boss`, revive, cooldown reset, grant, a heart's `team_powerup` and `team_boss`, an instant cast's `team_powerup` and `team_cast`: the write returns the error, members get `resync` with the state as it was, and a restart agrees |
  | `TestScanVerifierError` | a QR verifier error other than invalid or expired is returned as it is |
  | `TestInstantCastNoTeam` | an instant cast by someone in no team: `not_in_team` |
  | `TestLoadRefusesUnknownContent` | `Load` refuses stored state naming content the atlas no longer has (an atlas hiding it): a visited waypoint, a completed pattern, the selected shape, a cast's pattern, a stack scan's waypoint |
  | `TestTimerAfterTeamDeleted` | a timer firing after `TeamDeleted` (the fake clock's `leaky` stop) finds no entry and recreates nothing; `TeamDeleted` again, or for an unknown team, is a no-op |
  | `TestTimerAfterClose` | a timer firing after `Close` (`leaky`): nothing applied or published |
  | `TestPublishTeamGone` | an attack applied while teams no longer has the team (deleted, hook not yet run): applied in memory, published to nobody |
  | `TestLoadReadErrors` (`load_test.go`) | with a row in every per-team table, each of `Load`'s reads failing stops it (`load team state: …`), both ways: a table (or the stack's `team_scan.wrong`) missing, refused at prepare, and a view over it dividing by zero as rows stream; mended, the state loads as before |

- `tests/qrcooldown_test.go` (same fixture with `qrCooldown: true` through `qrStarted`, which skips if `game.qr_cooldown` is under 30 s, `minQRCooldown`):

  | Test | Covers |
  |---|---|
  | `TestQRCooldown` | `qr_cooldowns` `[]` before a cast; the quadrilateral's cast puts 2001, 2002, 2007, 2008 in it, each until `cast_at` + `game.qr_cooldown`; with the triangle selected, each scanned by a different member is `ErrWaypointOnCooldown` with no event, no version change, nothing logged; 2003 scans; 1 µs before `until` still refused; at `until` the timer publishes one `qr_cooldown_ended` (version + 1, list empty, the stack kept); 2001 then scans and the triangle casts, cooling its three waypoints from its cast |
  | `TestQRCooldownWriteCatchesUp` | with the timers off (`Close`), a scan at `until` publishes `qr_cooldown_ended` then `scan_added` and is accepted |
  | `TestQRCooldownAndAttackTogether` | `game.qr_cooldown` set to boss 2's interval: the quadrilateral defeats boss 1, so its cooldown ends exactly at boss 2's first attack: one event, `boss_attacked`, with `attacks` 1 and `qr_cooldowns` empty |
  | `TestQRCooldownFailedCast` | with the database closed, the quadrilateral's completing scan fails: `qr_cooldowns` stays empty, `cast_count` 0 |
  | `TestQRCooldownRestart` | a restart mid-cooldown rebuilds `qr_cooldowns` (JSON equal); a restart after it ended (the store closed meanwhile) loads `[]`, and 2001 scans |
  | `TestQRCooldownLatestCastWins` | the quadrilateral, then a triangle one cooldown later, then a restart with three times the cooldown: 2007 and 2008 until the quadrilateral's end, 2001–2003 until the triangle's, sorted by `until` |
  | `TestQRCooldownOff` | `game.qr_cooldown` 0: `qr_cooldowns` `[]` after the quadrilateral and 2001 scans straight away |

- `tests/powerup_test.go` (same fixture; `grant`, `activate`, `active` helpers):

  | Test | Covers |
  |---|---|
  | `TestGrantPowerup` | each kind granted once (`powerup_granted`, version + 1, `held`), a second heart wasted (`granted` false, no event, same version); unknown kind, unknown team, lobby team; `PowerupByID` |
  | `TestDamageMultiplierPowerup` | not held, a member (`not_leader`); activated (`active`, no until), `powerup_active` while armed, armed across a restart; the next cast deals × `powerups.damage_multiplier`, `boosted`; the cast after is normal |
  | `TestHeartPowerup` | `full_health` at full health; after an attack +1 with `attacks`, `phase_started_at` and `next_attack_at` unchanged, kept across a restart; a heart of 5 stops at `game.health`; knocked out is `knocked_out` (granting still works) |
  | `TestShortQRCooldownPowerup` | the window's until; `powerup_active` while it runs; a cast in it is `short_qr_cooldown` and cools for `powerups.short_qr_cooldown`; a restart keeps the window; its cooldown ends (`qr_cooldown_ended`), then the window (`powerup_ended`); a cast after cools for `game.qr_cooldown` |
  | `TestShortQRWindowEndsWithAttack` | a window ending at boss 1's first attack: one `boss_attacked` carrying both |
  | `TestInstantCast` | unknown waypoint, no pattern, not held, a member; after the quadrilateral (class and waypoints cooling), with a selection and a scan, the quadrilateral instant-cast: `instant`, `boosted` by an armed multiplier, `already_completed`, by the leader, selection and stack purged, waypoints unchanged, cooling from the new cast; a resend `powerup_not_held`; `team_cast.instant`; the log across a restart |
  | `TestInstantCastLockedPattern` | config.toml's schedule (`unlocks: true`): a 5-point pentagon at the start: `pattern_locked`, nothing changed (the instant cast stays held) |
  | `TestPowerupsAcrossRestart` | held kinds, an armed multiplier and a running window survive a restart (JSON equal); a window that ended while down is gone |
  | `TestPowerupReconcile` | `held` cleared by hand: activating fails, memory reloads (`resync`) |
  | `TestConcurrentPowerups` | 8 concurrent grants grant one; 8 concurrent activations use one |
  | `TestActivateUnknownKind` | a kind past the four: `unknown_powerup` |
  | `TestInstantCastFinishes` | an instant cast defeating the last boss calls `OnFinished` once |
  | `TestFinishFailsNotFinished` (`failure_test.go`) | the final cast (by scan, and by instant cast) with its `team_cast` write failing: no `OnFinished`, the last boss still alive; the cast again finishes once |
  | `TestFinishCommittedThenFailed` | the same failure with every boss already defeated in Postgres (a write that committed, then reported an error): the reload finishes the team once |
  | `TestLoadRefusesBadPowerups` | `Load` refuses a `damage_multiplier` activated past the end of the cast log, and a kind the code doesn't have (the CHECK dropped to plant one) |

- `tests/qte_test.go` (same fixture, with a qte store on its clock; `quiz`, `answer` helpers):

  | Test | Covers |
  |---|---|
  | `TestAnswerQTE` | a member's right answer: `qte` (correct, granted, by), the powerup held, `qte_answered` with version + 1; a teammate's resend `qte_already_answered`, no event; a wrong answer to the next quiz: not granted, held unchanged |
  | `TestAnswerQTEWasted` | right while holding the kind: `granted` false, one held |
  | `TestAnswerQTEWindow` | 1 µs before `ends_at` accepted; exactly at it `qte_closed`; after an admin's `End` `qte_closed` |
  | `TestAnswerQTERefusals` | unknown QTE, a draft (`unknown_qte`), option 4 of 4 (`invalid_option`), no team, a lobby team (`not_in_game`), knocked out; nothing recorded |
  | `TestAnswerQTERestart` | the answer and its powerup survive a restart (JSON equal); answering again `qte_already_answered` |
  | `TestConcurrentAnswers` | every member answering at once: exactly one recorded |
  | `TestAnswerAfterEndInPostgres` | the QTE closed in Postgres by hand while memory has it open (an `End` mid-write): `qte_closed`, no event, no row, no powerup, also after a restart |
  | `TestAnswerQTEReconcile` | a row inserted by hand: the answer fails, memory reloads it (`resync`) |
  | `TestQTEStats` | answered, correct, granted, per option; JSON |

- `tests/attack_test.go` (same fixture with `attacks: true`; `expectType` reads the next event's type, `expectNoEvent` fails on a waiting one):

  | Test | Covers |
  |---|---|
  | `TestBossAttacks` | walkthrough 8: at the start full health, `attacks` 0, `phase_started_at` = start, `next_attack_at` one interval on, one timer pending; nothing 1 µs before the first interval; exactly at it `boss_attacked`, health − damage, the next one interval later; defeating boss 1 starts boss 2's phase at the cast with the health then; boss 2's attacks one interval at a time, then `knocked_out` (health 0, `next_attack_at` null, no timer); no attacks after; `Select` and `Scan` get `ErrKnockedOut`, `ClearSelection` works; `Revive`: full health, `attacks` 0, phase from now, one timer, `revived`; selecting works again |
  | `TestAttacksAcrossRestart` | `Close` stops every timer; with the store closed the clock passes three attacks and memory doesn't change; a restart applies all three at load (health, `attacks`, `next_attack_at`); after a revive and one more attack, a restart reproduces the view exactly (JSON, version aside) |
  | `TestWriteCatchesUpAttacks` | boss 1's damage set to 4 through the rules (`fixtureOptions.bossRules`); with the timers off (`Close`), a scan after the first attack publishes `boss_attacked` then `scan_added` and sees the lower health; after a knockout's worth of attacks a select is refused with `ErrKnockedOut` and publishes `knocked_out` |
  | `TestReviveErrors` | `Revive` of an unknown team → `ErrTeamNotFound`; of a lobby team → `ErrNotInGame` |

- `tests/admin_test.go` (same fixture):

  | Test | Covers |
  |---|---|
  | `TestResetCooldowns` | with the QR cooldown on: the quadrilateral puts `class_4` and its four waypoints on cooldown and selecting it again is `shape_on_cooldown`; `ResetCooldowns` empties both (`cooldowns_reset` event); a restart keeps the reset; the next cast starts new cooldowns |
  | `TestResetCooldownsErrors` | an unknown team → `ErrTeamNotFound`; a lobby team → `ErrNotInGame` |
  | `TestFinishedHook` | every boss at 1 HP: `Rules.OnFinished` is called once, with the team, only by the cast that defeats the last boss, and **outside the lock** (the hook calls `Revive`, which takes it); a restart finding the team still `IN_GAME` calls it again |
  | `TestRemovedDuringWrite` | every member write (select, clear, scan, activate, instant cast, answer) by a member an admin removed after the write's lookup (a stale `Teams.ForUser`): `not_in_team` from the check under the lock, nothing changed |
  | `TestTeamDeleted` | `TeamDeleted` stops the timer and forgets the state; the clock moving a day later recreates nothing; the team has no standing |
  | `TestStandings` | a team's bosses defeated, its latest defeat's time and its damage on the current boss |

- `tests/gameloop_test.go`: **the game-loop simulation**, its own fixture (`newLeague`, schema `teamstate_sim`): the 2026 seed with its boss attacks, **100** teams (`simTeams`) of every size from 1 to `teams.size`, all started at the fake clock's start, the teams store's `OnRosterChange` wired to `RosterChanged`, plus waypoint `2099` in no pattern (`simNowhere`: the sloppy teams' wrong scan, which the seed alone can't produce). It plays `config.toml`'s unlock schedule (`game.unlocks`). The **big** patterns are those bigger than its `0` entry (with the 2026 seed the quadrilaterals, pentagons and hexagons): one pick in five is one (`randomPattern`), so every team tries them before and after they unlock: while one is bigger than the team's `unlocked_points`, `lockedFor` expects `shape_locked` (or `knocked_out`, checked first). Every select and clear is made by the leader (the first active member: member 1 once an admin removed member 0). Team *i* plays strategy *i* mod 16 at size 1 + (*i* / 16) mod 6, so each strategy runs at several sizes:

  | Strategy | What it does |
  |---|---|
  | `steady` | casts a random pattern every 2 steps, members taking turns; every 7 steps tries a pattern of its cooldown class instead (`tryCooldown`): `shape_on_cooldown` if its shape is all that class, else a wrong scan before it could complete, then a clear |
  | `rush` | casts every step: defeats every boss, then casts for nothing |
  | `solo` | one member scans every point, every 3 steps |
  | `idle` | never plays: knocked out by boss 1 (10 attacks × 5 min), revived at step 110, attacked again |
  | `sloppy` | a wrong scan (`wrong_scan`, `poisoned`), a refused scan (`selection_poisoned`), a reselect, a cast |
  | `retrier` | sends every scan twice: the resend gets `waypoint_already_scanned` (`no_shape_selected` after the completing one) |
  | `quitter` | gives up half its attempts after one scan (`ClearSelection`: no selection, empty stack) |
  | `repeater` | casts the first castable pattern in atlas order (`repeated`): the spell and QR cooldowns move it along the first few patterns and back, so it repeats them (`already_completed`) |
  | `slowpoke` | casts every 8 steps: attacked mid-fight, knocked out, revived 3 steps later |
  | `racer` | two members (one member twice, for a team of one) scan the same point at once: exactly one accepted |
  | `shrinking` | an admin removes a member at step 40 (the leader, with member 1 as the new leader, for even teams), or is refused the last member (`ErrLastMember`); the removed member's scans get `ErrNotInTeam` |
  | `multiplier` | a stored multiplier (×2, ×1.5, ×0.5, ×3) scales every cast |
  | `pampered` | casts every 2 steps; every 4, if anything is cooling, an admin resets its cooldowns (`ResetCooldowns`) and it casts its last pattern again at once: in the same instant as the reset, which must still start new cooldowns |
  | `doomed` | casts every 2 steps until an admin deletes the team at step 30 (`teams.Delete` → `TeamDeleted`); from then on its select is `not_in_team` and it has no state |
  | `powered` | one step in four an admin grants a random powerup (wasted when held); otherwise the leader uses a held one: a multiplier (then boosted casts), a heart (or `full_health`), a short QR window (casts cooling for the league's 1 min, `simShortQRCooldown`, ending on the timer), an instant cast (preferring a pattern past a cooldown; one try in three, a big pattern bigger than its current unlock is `pattern_locked`); `powerup_active`, `powerup_not_held`, `not_leader` and `knocked_out` refusals. The model tracks held, armed and the window; `check` compares `powerups` every step |
  | `quizzer` | never casts (so boss 1 knocks it out). Every 5 steps (`simQTEEvery`) the league starts a quiz open for one step, its powerup and right option turning; a quizzer answers right half the time, sometimes two members at once (exactly one recorded) or with a teammate's resend (`qte_already_answered`); one time in four it answers a step late (`qte_closed`); knocked out, `knocked_out`. The model tracks the latest answer and held kinds; `check` compares `qte` every step. After a restart with no downtime, a quiz started at step 45 is still open; after any downtime it's gone |

  | Test | Covers |
  |---|---|
  | `TestGameLoopSimulation` | **180 steps of 30 s** (90 min of game time). Each step moves the fake clock (firing every due attack), then all 100 teams act at once by strategy, every call's error checked. After each step every team's state is compared with **`model`**, an independent re-implementation of the rules: `bosses_defeated`, `boss` (serial, damage, `attacks`, `phase_started_at`, `next_attack_at`), `health`, `knocked_out`, `damage_multiplier`, `cast_count`, `unlocked_points` (`model.unlocked`: the schedule's value at the largest key the team has reached) and whether the quadrilateral, pentagon and hexagon are in `locked_shapes` (each iff `unlocked_points` is below its size), `qr_cooldowns` (`model.qr`, each waypoint's latest cast + the league's cooldown); and each cast's `dealt`, `boss`, `defeated_boss`, `already_completed`. A team through every boss is `FINISHED` (the league wires `Rules.OnFinished` to teams' `Finish`, as `server.Build` does): from then on its select must be `not_in_game`. A deleted team (`doomed`, wired through `OnDelete`) must have no state and a `not_in_team` select. At the end, every team's watcher (a member never removed, subscribed to the hub) must hold the team's state as its last `team_state`, version included (`converge`, deleted teams aside), and every team's leaderboard entry (a `leaderboard.Leaderboard` over the league) must match the model's ranking: rank, bosses, damage, finished (`checkLeaderboard`). Counters fail the test if any configuration never happened, finished-team refusals, cooldown resets, deletions and deleted-team refusals included. Patterns with a cooling waypoint aren't picked (`castable`); a team with none to cast waits for a later step (counted), and an unlocked team that picked a big pattern waits for its waypoints rather than casting something small. One select in three probes a cooling waypoint (`probeQR`), which must be `ErrWaypointOnCooldown`; `tryCooldown` expects it too when its pattern has one. The league's QR cooldown is **`simQRCooldown` (2 min)**: the seed's 10 waypoints would leave config.toml's 5 min too few casts for any team to reach every boss or a pentagon or hexagon. The league's bosses attack every **`simAttackInterval` (5 min)** with `config.toml`'s damages (shorter than the configured 10 min, so 90 minutes see many attacks and knockouts, yet the fastest teams still get through every boss). Last run (2026 seed, before `game.unlocks` replaced the single unlock at boss 5, so today's counts differ): 1,194 casts, 3 teams through every boss, 67 wrong scans (all through `2099`), 5 big casts after the unlock, 107 knockouts, and every other counter above zero (run with `-v` for all of them). It found the knockout-cap bug ("Boss attacks and health") and the time-based cooldown reset's bug (a cast in the reset's instant was dropped; "Cooldown reset" in Concepts) |
  | `TestGameLoopRestart` | The same league, backend down part way: **45 steps**, then the store waits for every event (`converge`), is closed (no timers fire), the clock moves on by the downtime, and a new store loads in a new epoch; then **45 more steps**. Subtests per downtime: **0** (every view must survive exactly, JSON with version aside), **90 s** (shorter than the league's attack interval, `simAttackInterval`), **15 min** (several attacks of every boss), **1 h** (must knock teams out while down). After the restart every team must equal its `model` at the new time, so the attacks that fell due while down are applied at load, knockouts included; each watcher gets the new store's `Snapshot`, as a reconnecting phone would, and every phone must converge at the end, and every team's leaderboard rank must match its model (`checkLeaderboard`; deleted teams have no state to compare and are left out). Last run (race detector, ~10 s for all four): knocked out while down 0, 12, 54 and 61 (2026 seed) (90 s can knock out a team at its last health point with an attack due) |
  | `BenchmarkGameLoopCast` | casts per second with the 100 teams playing at once, boss attacks off: each goroutine owns a team and casts random patterns (a select plus every scan, each a Postgres write). **433,873 ns per cast** (`-benchtime 3s`, 16 CPUs, local Postgres 17), about 2,300 casts or 11,000 writes a second |

- `tests/publish_bench_test.go`: **`BenchmarkTeamStatePublish`**, parallel `PublishTeam` calls across 1,666 `IN_GAME` teams (10,000 players at `teams.size` 6; an in-memory `Teams`, so no team rows), every member with an open subscription. It measures the lock, encoding once and queueing for each member: about **1.0 µs** a publish on a 10-core laptop, down from 4.4 µs before the sharded hub and roster cache (`internal/platform/README.md`, "Performance"). Measured before the cast log and `poisoned` were added to the `View`; a team with no casts encodes the same size plus `"poisoned":false`.
- `internal/server/tests/teamstate_test.go`:

  | Test | Covers |
  |---|---|
  | `TestSelectAndScanRoutes` | every route and error code over HTTP: `not_in_game`, `not_in_team`, `invalid_body` (the body size limit included), `invalid_qr` (tampered) and `qr_expired` (stale) for a scan, `unknown_shape`, `unknown_waypoint`, `no_shape_selected`, `waypoint_already_scanned`; one member scanning two waypoints; a quadrilateral completing; a wrong scan (`wrong_scan`, `poisoned`, `stack[].wrong`), then `selection_poisoned`, then a reselect clearing it. Scans submit tokens minted with the test secret (`qrToken`); after the quadrilateral's cast, reselecting the quadrilateral is `409 shape_on_cooldown` |
  | `TestCastLogRoute` | `GET /api/teams/me/casts`: `404 not_in_team`, `[]` before any cast, then the quadrilateral, a triangle and the quadrilateral again (the cooldown forbids two quadrilaterals in a row), newest first with `already_completed`, `shape`, `class` and `damage` |
  | `TestQRCooldownOverHTTP` | two started teams over HTTP, `config.toml`'s `game.qr_cooldown`: A's quadrilateral puts its 4 waypoints in `qr_cooldowns` (each until `cast_at` + `game.qr_cooldown`) in the response and the stream's cast event; with the triangle selected, A scanning 2001 is `409 waypoint_on_cooldown` with no event, 2003 is accepted; B scans 2001 (per team); a restart keeps A's list and 2002 is still refused |
  | `TestSpellCooldownOverHTTP` | two started teams over HTTP: A's quadrilateral sets `cooldown_class` `class_4` and `cooldown_shapes` in the response and the stream's cast event; A reselecting the quadrilateral is `409 shape_on_cooldown` with no event; B still selects the quadrilateral (the cooldown is per team); a restart keeps A's cooldown; A's triangle moves it to `class_3`, and the quadrilateral casts again |
  | `TestGameBosses` | `GET /api/game/bosses`: `401 missing_token`; `[]` before content; after seeding, `game.bosses` bosses in serial order with name, asset and HP; a started team's state shows boss 1 at 0 damage, multiplier 1; after a quadrilateral over HTTP, `last_cast` hit boss 1 with `dealt` = `damage` and `boss.damage` = `dealt` |
  | `TestGameShapes` | `GET /api/game/shapes`: `401 missing_token` without a token; the seed's 5 shapes, `quadrilateral` with 210 patterns of 4 points, a valid damage range per shape, `max_pattern_points` from `config.toml`, and no `scan_window_ms` |

- `internal/server/tests/simulation_test.go`: simulated phones, each with its own request and response delays and its own event stream delivered after that delay:

  | Test | Covers |
  |---|---|
  | `TestSimTeamCompletesUnderLatency` | four players scan the quadrilateral at once with delays up to 150 ms each way: exactly one `pattern_completed`; every screen ends with an empty stack and the quadrilateral completed |
  | `TestSimRaceSameWaypoint` | two players scan the same waypoint at once: one `200`, one `409 waypoint_already_scanned`; every screen converges on a one-scan stack |
  | `TestSimWrongScanReachesEveryone` | a wrong scan racing a teammate's correct one: the wrong one gets `wrong_scan`, the other is accepted or gets `selection_poisoned`; every phone sees `poisoned`; a reselect clears it on every phone |
  | `TestSim100Teams` | **100 teams** of every size at once over real HTTP and event streams, each player on its own delays (0–30 ms each way), 6 rounds each by one of 8 strategies (`steady`, `solo`, `sloppy` with waypoint `2099`, in no pattern, `racer`, `retrier`, `quitter`, `shrinking` with an admin removal over HTTP, `powered` granting and using powerups over HTTP); bosses never attack and the QR cooldown is off (back-to-back rounds reuse the seed's waypoints). Every request's status and code is checked; at the end each team's `cast_count` is the casts it made, every phone holds the team's state (same `version`, kept newest-by-version like the frontend), and every removed player got `no_team`. Each team picks its next pattern of another size than its last cast (the spell cooldown). It plays `config.toml`'s unlock schedule: each round it reads its state, tries selecting the first of its `locked_shapes` (`409 shape_locked`) and picks a pattern no bigger than its `unlocked_points` |

- `internal/server/tests/admin_test.go`:

  | Test | Covers |
  |---|---|
  | `TestAdminRemoveFromStartedTeam` | after an admin removes a member of a started team: their stream gets `no_team` and no further `team_state`; their scan, selection and state read get `404 not_in_team`; the rest complete a quadrilateral using the removed player's earlier scan |
  | `TestAdminRemoveRacesScan` | a member's scan racing their removal, for every non-leader: the removal always succeeds; the scan is accepted (and stays in the stack) or refused with `not_in_team`, never accepted after the removal. Which wins isn't controlled; the refused path is covered deterministically above |
  | `TestAdminRevive` | a started team's state has `health` = `game.health`, `knocked_out` false and a `next_attack_at`; a stored phase with 0 health, applied at restart, knocks it out (`health` 0, `next_attack_at` null); select and scan get `409 knocked_out`; the revive route: `403 not_admin` for a player, `404 team_not_found`, `409 not_in_game` for a lobby team; an admin's revive returns full health and `attacks` 0, a member's stream gets `revived`, and selecting works again |
  | `TestAdminResetCooldownsOverHTTP` (`admin_test.go`) | the quadrilateral cools its class and waypoints (a cooled waypoint is `waypoint_on_cooldown`); the reset route's guard, `invalid_team_id`, `team_not_found`, `not_in_game`; the reset empties `cooldown_class` and `qr_cooldowns`, a member's stream gets `cooldowns_reset`, and the waypoint scans again |
  | `TestPowerupsOverHTTP` (`admin_test.go`) | the admin grant (every refusal, `powerup_granted`, a wasted grant) and the player route (every refusal, the multiplier, an instant cast boosted, a short window's until); the admin overview shows `powerups` |
  | `TestLeaderboardAndFinish` (`admin_test.go`) | every boss at 1 HP: a team through every boss becomes `FINISHED` (`team` of type `finished`) and its select is `not_in_game`; the leaderboard puts it first, `finished` |

- `internal/server/tests/events_test.go`: `team_state` on connect; `TestStartPublishesTeamState`: when the leader starts, every open stream gets `team` then the empty `team_state`, in that order.
