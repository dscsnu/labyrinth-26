# Spells: damage model and offline generator

The `spells` package owns the mathematical modeling for spell damage in **Labyrinth 2026**: equirectangular campus projection, shortest closed Euclidean tour (TSP), Andrew's monotone chain convex hull, and calibrated damage calculation.

Damage values are **precomputed offline** and stored in the database (`pattern.damage`), loaded once into memory at startup (`atlas.Load`), and read as a plain field during gameplay. The game server **never imports** this package.

Tunables appear with their `config.toml` key: **1.5** (`spell.walkability`), **0.8** (`spell.per_100m`), `spell.base` (**1: 15, 2: 22, 3: 58, 4: 186, 5: 384, 6: 1200**), and **75** (`spell.bonus.concave_quad`).

---

## 1. Concepts

| Term | Meaning |
|---|---|
| `Waypoint` | A physical QR location identified by its 4-digit ID (`2001`–`2010` in 2026) and GPS coordinates (`latitude`, `longitude`). |
| `Pattern` | A valid set of 1–6 waypoints forming a shape type. In 2026 every combination of 2–6 checkpoints is one, shaped by its size (line, triangle, quadrilateral, pentagon, hexagon). |
| `Coordination Count (k)` | The number of distinct waypoints in a pattern ($k \in [1, 6]$), corresponding to the number of teammates who had to be in position. |
| `Projection` | Equirectangular projection from WGS 84 GPS coordinates to local planar metric coordinates (metres east $X$ and north $Y$) centered on the waypoints' mean origin. |
| `TourM` ($P_{\text{tour}}$) | Shortest closed Euclidean loop connecting all waypoints in the pattern (0 for 1 point, out-and-back for 2 points, Traveling Salesperson closed loop for $\ge 3$ points). |
| `WalkM` ($P_{\text{adj}}$) | Realistic campus walking distance in metres: $P_{\text{tour}} \times \text{walkability}$ (`spell.walkability`). |
| `Damage` | Integer score dealt by casting the spell. Stored in `pattern.damage`. |
| `DamageRules` | The formula's coefficients, built from `config.toml` (`spell.base`, `spell.bonus.concave_quad`, `spell.per_100m`, `spell.walkability`, and `game.max_pattern_points` as `MaxPoints`). `DamageRules.Damage(pts)` returns a pattern's damage, or an error for a size outside 1–`MaxPoints` or one with no `spell.base` entry (never a silent 0). |
| `Generator` | The offline CLI tool `cmd/spells` that reads coordinates and patterns from Postgres and writes `seeds/2026_spells.sql`. |
| `Drift Test` | The automated test in `internal/spells/tests/` that verifies stored damage in Postgres matches recomputed damage from current coordinates and config. |

---

## 2. The Damage Formula

For any pattern $\sigma$ with $k$ waypoints, shortest closed tour $P_{\text{tour}}$ (in metres), and concavity:

$$D(\sigma) = \operatorname{round}\left(\text{Base}(k) + \text{Bonus}(\text{concave}) + b \cdot \frac{\alpha \cdot P_{\text{tour}}}{100}\right)$$

except for the biggest size, $k$ = `game.max_pattern_points` (**6**), which deals its base flat, with no bonus and no distance term:

$$D(\sigma) = \text{Base}(k) \quad \text{for } k = \texttt{game.max\_pattern\_points}$$

### Calibrated Parameters

| Parameter | Config Key | Default | Purpose |
|---|---|:---:|---|
| $\text{Base}(k)$ | `spell.base` | **22, 58, 186, 384, 1200** | Base coordination damage by pattern size $k \in [2, 6]$ (and 15 for $k=1$). Sets tier power. The biggest size, `game.max_pattern_points` (**6**), deals its base flat (**1200**), with no distance term. |
| $\text{Bonus}$ | `spell.bonus.concave_quad` | **75** | Difficulty bonus awarded to concave quadrilaterals (4 points whose convex hull has 3 vertices). |
| $b$ | `spell.per_100m` | **0.8** | Distance damage per 100 metres of walking. Rewards teams for physical coverage across campus. |
| $\alpha$ | `spell.walkability` | **1.5** | Campus detour index ($\frac{\text{Walking Distance}}{\text{Euclidean Distance}}$). Real footpaths require walking $\approx 1.5\times$ straight-line distance due to buildings and terrain. |

### Anti-Exploit Design
1. **Distance Exploitation Prevention:** If damage scaled only with distance, two runners could sprint back and forth between two distant checkpoints repeatedly (Arcade $\leftrightarrow$ Lake, $2002 \leftrightarrow 2007$), ignoring team coordination. The $\text{Base}(k)$ term ensures multi-player shapes are strictly more rewarding.
2. **Corner Camping Prevention:** If damage scaled only with player count $k$, teams would cluster in a single building or lawn, scanning 4 or 5 points 10 metres apart. The $b \cdot \frac{\alpha \cdot P_{\text{tour}}}{100}$ term demands real campus coverage to maximize damage.
3. **Concavity Reward:** Concave quadrilaterals require positioning a team member deep in an interior building surrounded by hull vertices; the $+75$ bonus recognizes this coordination challenge.

---

## 3. Geometric Modeling

### A. Equirectangular Projection
GPS coordinates are converted to planar metric coordinates $(X, Y)$ in metres centered on the arithmetic mean $(\phi_0, \lambda_0)$ of all campus waypoints:

$$X = R \cdot (\lambda - \lambda_0) \cdot \cos(\phi_0)$$
$$Y = R \cdot (\phi - \phi_0)$$

where:
- $\phi, \lambda$ are latitude and longitude in radians.
- $R = 6,371,000.0\text{ m}$ is Earth's volumetric mean radius (IUGG recommendation; constant `EarthRadiusM`).
- $X$ is metres east of the campus barycenter.
- $Y$ is metres north of the campus barycenter.

### B. Shortest Closed Tour ($P_{\text{tour}}$)
- **$k = 1$:** Tour is 0 m (single waypoint; damage is $\text{Base}(1) = 15$).
- **$k = 2$:** Out and back: $2 \times \operatorname{Distance}(p_1, p_2)$.
- **$k \in [3, 6]$:** The shortest closed loop visiting every point once and returning to the start. Since $k \le 6$, all $(k-1)!$ cyclic orderings are evaluated (at most $5! = 120$ permutations).

### C. Convex Hull
Strict convex hull vertices are computed using Andrew's Monotone Chain algorithm. No three of the 10 2026 checkpoints are collinear (minimum triangle cross-product $310.98\text{ m}^2$, from `damage_lookup.json`'s projected coordinates). Only three (`2006` AIC, `2008` Canopies, `2010` CnD Atrium) ever appear as interior points in concave subsets; the other 7 always lie on the outer boundary.

---

## 4. Worked Example: Quadrilateral Pattern 188

Pattern `188` in the 2026 seed is a `quadrilateral` of four checkpoints, a **concave** one (`damage_lookup.json`'s `concave_quad`): Canopies lies inside the triangle of the other three. Projected coordinates are `damage_lookup.json`'s `local_xy_m` (centred on the 10 checkpoints' mean):
- `2001` G Block Junc: `(28.5275017, 77.5748206)` $\to X = 42.082\text{ m}, Y = 207.137\text{ m}$
- `2002` Arcade: `(28.5274593, 77.5728646)` $\to X = -149.012\text{ m}, Y = 202.423\text{ m}$
- `2007` Lake: `(28.524414, 77.5768289)` $\to X = 238.285\text{ m}, Y = -136.199\text{ m}$
- `2008` Canopies: `(28.5263552, 77.5743113)` $\to X = -7.675\text{ m}, Y = 79.652\text{ m}$

### Steps:
1. **$k = 4$ waypoints**; the convex hull is Arcade, Lake, G Block Junc (3 points: Canopies is interior), so concave $\implies \text{Base} = 186$, $\text{Bonus} = 75$ (`spell.bonus.concave_quad`).
2. **Shortest closed tour loop:** G Block Junc $\to$ Arcade $\to$ Canopies $\to$ Lake $\to$ G Block Junc:
   - $2001 \to 2002 = 191.15\text{ m}$
   - $2002 \to 2008 = 187.21\text{ m}$
   - $2008 \to 2007 = 327.24\text{ m}$
   - $2007 \to 2001 = 395.44\text{ m}$
   - $P_{\text{tour}} = 191.15 + 187.21 + 327.24 + 395.44 = \mathbf{1,101.05\text{ m}}$.
3. **Adjusted walking distance:**
   - $P_{\text{adj}} = 1.5 \times 1,101.05 = \mathbf{1,651.58\text{ m}}$.
4. **Damage calculation:**
   - $\text{Damage} = \operatorname{round}\left(186 + 75 + 0.8 \times \frac{1,651.58}{100}\right) = \operatorname{round}(274.21) = \mathbf{274}$, `damage_lookup.json`'s value.

Stored in Postgres as:
```sql
UPDATE pattern SET damage = 274 WHERE id = 188;
```

---

## 5. Combinatorics and Damage Ranges

Across every subset of the 10 2026 checkpoints of size 2 to 6 ($\sum_{k=2}^{6} \binom{10}{k} = 837$), from `damage_lookup.json`:

| $k$ | Shape Classification | Total Combinations | Convex | Concave | Actual Damage Range | Target Range |
|:---:|---|:---:|:---:|:---:|:---:|:---:|
| **2** | Line Segment | 45 | 45 | 0 | 25 – 36 (median 29) | $30 \pm 5$ |
| **3** | Triangle | 120 | 120 | 0 | 64 – 75 (median 70) | $70 \pm 15$ |
| **4** | Convex Quad | 156 | 156 | 0 | 195 – 205 (median 200) | $200 \pm 25$ |
| **4** | Concave Quad | 54 | 0 | 54 | 270 – 278 (median 275) | $275 \pm 30$ |
| **5** | Pentagon | 252 | 101 | 151 | 395 – 403 (median 400) | $400 \pm 40$ |
| **6** | Hexagon | 210 | 28 | 182 | 1200 (fixed) | 1200 |

These are the 2026 seed's patterns exactly: `seeds/2026.sql` makes every combination a pattern (837), shaped by size alone (the game's `quadrilateral` shape holds both the convex and the concave quads; concavity changes only the damage). Every stored damage equals `damage_lookup.json`'s (`TestSeedMatchesDamageLookup`). *(2025's seed had 359 hand-reviewed patterns of sizes 3–4 over 11 other waypoints.)*

---

## 6. How and When to Run the Generator

The offline generator lives in `backend/cmd/spells/main.go`.

### When to run:
- Any waypoint's coordinates in `waypoint` are added or updated.
- New patterns are added to the `pattern` table.
- Spell coefficients in `config.toml` (`[spell]`) are changed.

### How to run:
Run it from `backend/` (it refuses anywhere else), with the backend stopped. It reads `DATABASE_URL` from the environment or `.env`, and `config.toml` (or `$CONFIG_FILE`) for `[spell]`:
```bash
cd backend
go run . seed              # the content (seeds/2026.sql); damage may still be stale
go run ./cmd/spells        # rewrites internal/platform/postgres/seeds/2026_spells.sql from it
go run . seed              # applies the new damage
```
Commit the regenerated seed file: the drift test compares it against a fresh computation.

### What it does:
1. Reads every waypoint (`id`, `latitude`, `longitude`) and pattern from Postgres at `DATABASE_URL`, which is **required**: the 2026 patterns are generated by the seed's SQL (every combination), so a seeded database is the one source.
2. Builds one `DamageRules` from `config.toml`.
3. Computes every pattern's damage with `DamageRules.Damage`; a pattern it can't score stops the run with its ID, writing nothing.
4. Overwrites `backend/internal/platform/postgres/seeds/2026_spells.sql` with deterministic `UPDATE pattern SET damage = ... WHERE id = ...;` statements (837 of them).
5. `Seed2026` applies this file right after `2026.sql`, ensuring fresh databases have damage populated.

---

## 7. The Drift Test

The test `TestPatternDamageDrift` in `internal/spells/tests/spells_test.go`:
1. Connects to test Postgres via `pgtest.Open` and seeds it with `Seed2026`.
2. Loads all waypoints and patterns.
3. Recomputes every pattern's damage using `internal/spells` and `configtest.Settings(t)`.
4. Compares computed damage to the stored `pattern.damage` column.

**If any coordinate or config value changes without re-running `go run ./cmd/spells`, the drift test fails immediately in CI.**

## 8. The Damage Lookup Check

`damage_lookup.json` (`internal/spells/testdata/`) is the source of truth for 2026 damage: every combination of 2 to 6 of the 10 2026 checkpoints, with its tour, hull and damage. Nothing reads it at runtime; the game reads `pattern.damage`. `TestDamageLookupMatchesConfig` keeps it and `config.toml` in step:
1. Its `damage_coefficients` must equal `config.toml`'s `[spell]` values.
2. Every combination, recomputed from the JSON's own checkpoint coordinates with `DamageRules`, must have the JSON's damage (837 of 837 today).

Retuning means changing both together: edit `[spell]` and regenerate the JSON (or the reverse), then re-run `go run ./cmd/spells` for the seed.

## 9. Tests

| Test | Covers |
|---|---|
| `TestPureMathGeometry` | No database: a 1-point pattern deals `spell.base.1`; a 2-point tour is out and back; a 100 m square's tour; the convex hull drops an interior point; a concave quad gets exactly `spell.bonus.concave_quad` more; doubling `spell.per_100m` adds the formula's distance term; the biggest size (`game.max_pattern_points`) deals `spell.base` flat; an empty pattern, one past `MaxPoints`, and a size missing from `spell.base` are errors |
| `TestSeedPatternGeometryProperties` | The 2026 seed: 837 patterns, positive damage, hull no bigger than the pattern, pattern 1's (the line G Block Junc – Arcade) tour within 1 m of `damage_lookup.json`'s 382.30 m, every stored damage equal to `DamageRules.Damage` |
| `TestPatternDamageDrift` | Every stored `pattern.damage` equals a fresh computation from the seed's coordinates and `config.toml`; lists each drifted pattern and says to re-run `go run ./cmd/spells` |
| `TestDamageLookupMatchesConfig` | `damage_lookup.json`'s coefficients equal `[spell]`, and all 837 of its combinations recompute to its damage |
| `TestSeedMatchesDamageLookup` | The 2026 seed is `damage_lookup.json`: its waypoints are the JSON's checkpoints (by name, same coordinates), and every one of the 837 seeded patterns has the JSON's damage for the same set of checkpoints |
