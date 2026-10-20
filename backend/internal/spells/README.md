# Spells: damage model and offline generator

The `spells` package owns the mathematical modeling for spell damage in **Labyrinth 2026**: equirectangular campus projection, shortest closed Euclidean tour (TSP), Andrew's monotone chain convex hull, and calibrated damage calculation.

Damage values are **precomputed offline** and stored in the database (`pattern.damage`), loaded once into memory at startup (`atlas.Load`), and read as a plain field during gameplay. The game server **never imports** this package.

Tunables appear with their `config.toml` key: **1.5** (`spell.walkability`), **0.8** (`spell.per_100m`), `spell.base` (**1: 15, 2: 22, 3: 58, 4: 186, 5: 384, 6: 1200**), and **75** (`spell.bonus.concave_quad`).

---

## 1. Concepts

| Term | Meaning |
|---|---|
| `Waypoint` | A physical QR location identified by its 4-digit ID (`1001`–`1011`) and GPS coordinates (`latitude`, `longitude`). |
| `Pattern` | A valid set of 1–6 waypoints forming a shape type (e.g. square, triangle). |
| `Coordination Count (k)` | The number of distinct waypoints in a pattern ($k \in [1, 6]$), corresponding to the number of teammates who had to be in position. |
| `Projection` | Equirectangular projection from WGS 84 GPS coordinates to local planar metric coordinates (metres east $X$ and north $Y$) centered on the waypoints' mean origin. |
| `TourM` ($P_{\text{tour}}$) | Shortest closed Euclidean loop connecting all waypoints in the pattern (0 for 1 point, out-and-back for 2 points, Traveling Salesperson closed loop for $\ge 3$ points). |
| `WalkM` ($P_{\text{adj}}$) | Realistic campus walking distance in metres: $P_{\text{tour}} \times \text{walkability}$ (`spell.walkability`). |
| `Damage` | Integer score dealt by casting the spell. Stored in `pattern.damage`. |
| `DamageRules` | The formula's coefficients, built from `config.toml` (`spell.base`, `spell.bonus.concave_quad`, `spell.per_100m`, `spell.walkability`, and `game.max_pattern_points` as `MaxPoints`). `DamageRules.Damage(pts)` returns a pattern's damage, or an error for a size outside 1–`MaxPoints` or one with no `spell.base` entry (never a silent 0). |
| `Generator` | The offline CLI tool `cmd/spells` that reads coordinates and patterns and writes `seeds/dev_2025_spells.sql`. |
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
1. **Distance Exploitation Prevention:** If damage scaled only with distance, two runners could sprint back and forth between two distant buildings repeatedly ($1010 \leftrightarrow 1002$), ignoring team coordination. The $\text{Base}(k)$ term ensures multi-player shapes are strictly more rewarding.
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
Strict convex hull vertices are computed using Andrew's Monotone Chain algorithm. All 11 campus checkpoints are non-collinear (minimum triangle cross-product is $1,980.47\text{ m}^2$). Out of the 11 checkpoints, only three (`1003` A, `1004` CnD, `1008` C1) ever appear as interior points in concave subsets; the other 8 always lie on the outer boundary.

---

## 4. Worked Example: Square Pattern 87

Pattern `87` in `dev_2025.sql` is a `square` composed of four waypoints:
- `1001` Tower 6 FruitShop (T6): `(28.52836141567019, 77.57777379378554)` $\to X = 340.428\text{ m}, Y = 309.178\text{ m}$
- `1002` Research Block (R): `(28.527471246410833, 77.57890336254033)` $\to X = 450.779\text{ m}, Y = 210.196\text{ m}$
- `1007` UAC (SARC): `(28.523582249548888, 77.57437275274097)` $\to X = 8.155\text{ m}, Y = -222.241\text{ m}$
- `1008` Cluster 1 (C1): `(28.52440905859402, 77.57308311017349)` $\to X = -117.838\text{ m}, Y = -130.304\text{ m}$

### Steps:
1. **$k = 4$ waypoints**, convex $\implies \text{Base} = 186$, $\text{Bonus} = 0$.
2. **Shortest closed tour loop:** $1001 \to 1002 \to 1007 \to 1008 \to 1001$:
   - $1001 \to 1002 = 148.01\text{ m}$
   - $1002 \to 1007 = 618.91\text{ m}$
   - $1007 \to 1008 = 155.80\text{ m}$
   - $1008 \to 1001 = 635.24\text{ m}$
   - $P_{\text{tour}} = 148.01 + 618.91 + 155.80 + 635.24 = \mathbf{1,557.96\text{ m}}$.
3. **Adjusted walking distance:**
   - $P_{\text{adj}} = 1.5 \times 1,557.96 = \mathbf{2,336.93\text{ m}}$.
4. **Damage calculation:**
   - $\text{Damage} = \operatorname{round}\left(186 + 0 + 0.8 \times \frac{2,336.93}{100}\right) = \operatorname{round}(204.695) = \mathbf{205}$.

Stored in Postgres as:
```sql
UPDATE pattern SET damage = 205 WHERE id = 87;
```

---

## 5. Combinatorics and Damage Ranges

Across all 1,474 possible subsets of the 11 campus checkpoints ($\sum_{k=2}^{6} \binom{11}{k} = 1,474$):

| $k$ | Shape Classification | Total Combinations | Convex | Concave | Actual Damage Range | Target Range |
|:---:|---|:---:|:---:|:---:|:---:|:---:|
| **2** | Line Segment | 55 | 55 | 0 | 26 – 47 (median 32) | $30 \pm 5$ |
| **3** | Triangle | 165 | 165 | 0 | 64 – 85 (median 75) | $70 \pm 15$ |
| **4** | Convex Quad | 262 | 262 | 0 | 196 – 215 (median 208) | $200 \pm 25$ |
| **4** | Concave Quad | 68 | 0 | 68 | 272 – 288 (median 282) | $275 \pm 30$ |
| **5** | Pentagon | 462 | 235 | 227 | 397 – 413 (median 408) | $400 \pm 40$ |
| **6** | Hexagon | 462 | 123 | 339 | 1200 (fixed) | 1200 |

*(Note: The 2025 dev seed implements 359 hand-reviewed patterns across sizes 3–4).*

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
go run ./cmd/spells        # rewrites internal/platform/postgres/seeds/dev_2025_spells.sql
go run . seed              # applies it to the database
```
Commit the regenerated seed file: the drift test compares it against a fresh computation.

### What it does:
1. Reads every waypoint (`id`, `latitude`, `longitude`) and pattern from Postgres at `DATABASE_URL`. Without `DATABASE_URL`, it parses them from `seeds/dev_2025.sql` instead (and logs that it did); both give the same patterns for the committed seed.
2. Builds one `DamageRules` from `config.toml`.
3. Computes every pattern's damage with `DamageRules.Damage`; a pattern it can't score stops the run with its ID, writing nothing.
4. Overwrites `backend/internal/platform/postgres/seeds/dev_2025_spells.sql` with deterministic `UPDATE pattern SET damage = ... WHERE id = ...;` statements.
5. `SeedDev2025` applies this file right after `dev_2025.sql`, ensuring fresh databases have damage populated.

---

## 7. The Drift Test

The test `TestPatternDamageDrift` in `internal/spells/tests/spells_test.go`:
1. Connects to test Postgres via `pgtest.Open` and seeds it with `SeedDev2025`.
2. Loads all waypoints and patterns.
3. Recomputes every pattern's damage using `internal/spells` and `configtest.Settings(t)`.
4. Compares computed damage to the stored `pattern.damage` column.

**If any coordinate or config value changes without re-running `go run ./cmd/spells`, the drift test fails immediately in CI.**

## 8. The Damage Lookup Check

`damage_lookup.json` (repository root) is the source of truth for 2026 damage: every combination of 2 to 6 of the 10 2026 checkpoints, with its tour, hull and damage. Nothing reads it at runtime; the game reads `pattern.damage`. `TestDamageLookupMatchesConfig` keeps it and `config.toml` in step:
1. Its `damage_coefficients` must equal `config.toml`'s `[spell]` values.
2. Every combination, recomputed from the JSON's own checkpoint coordinates with `DamageRules`, must have the JSON's damage (837 of 837 today).

Retuning means changing both together: edit `[spell]` and regenerate the JSON (or the reverse), then re-run `go run ./cmd/spells` for the seed.

## 9. Tests

| Test | Covers |
|---|---|
| `TestPureMathGeometry` | No database: a 1-point pattern deals `spell.base.1`; a 2-point tour is out and back; a 100 m square's tour; the convex hull drops an interior point; a concave quad gets exactly `spell.bonus.concave_quad` more; doubling `spell.per_100m` adds the formula's distance term; the biggest size (`game.max_pattern_points`) deals `spell.base` flat; an empty pattern, one past `MaxPoints`, and a size missing from `spell.base` are errors |
| `TestSeedPatternGeometryProperties` | The 2025 seed: 359 patterns, positive damage, hull no bigger than the pattern, square 87's tour within 1 m of 1,557.96 m, every stored damage equal to `DamageRules.Damage` |
| `TestPatternDamageDrift` | Every stored `pattern.damage` equals a fresh computation from the seed's coordinates and `config.toml`; lists each drifted pattern and says to re-run `go run ./cmd/spells` |
| `TestDamageLookupMatchesConfig` | `damage_lookup.json`'s coefficients equal `[spell]`, and all 837 of its combinations recompute to its damage |
