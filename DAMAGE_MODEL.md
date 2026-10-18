# Labyrinth 2026 — Spell Damage Model & Geometry Specification

This document outlines the mathematical model, formula terms, design rationale, and integration guide for spell damage calculations in **Labyrinth 2026**.

All 1,474 combinations of checkpoints across the campus have been precomputed and stored in [`damage_lookup.json`](./damage_lookup.json).

---

## 1. The Damage Formula

For any combination of waypoints $\sigma$, the total damage dealt by the cast spell is calculated as:

$$D(\sigma) = \operatorname{round}\left(a \cdot k + b \cdot \frac{\alpha \cdot P_{\text{tour}}}{100}\right)$$

### Calibrated Parameter Values
* $a = 15$ (Coordination Scale)
* $b = 2$ (Distance Scale)
* $\alpha = 1.5$ (Campus Walkability Multiplier)

---

## 2. Term Breakdown & Definitions

| Term | Symbol / Value | Description & Purpose |
|---|:---:|---|
| **Spell Pattern** | $\sigma$ | The unordered set of checkpoints visited (e.g. `{"A", "CnD", "R", "T6"}`). Keyed canonically as alphabetically sorted names joined by hyphens (`"A-CnD-R-T6"`). |
| **Coordination Scale** | $k \in [2, 6]$ | The number of distinct waypoints involved in the pattern, corresponding to the number of synchronized team members required to complete the cast. |
| **Coordination Weight** | $a = 15$ | Base damage awarded per player. Directly rewards the tactical and communication overhead needed to position multiple players across campus simultaneously. |
| **Ideal Perimeter** | $P_{\text{tour}}$ (meters) | The shortest closed Euclidean loop (Traveling Salesperson Tour) connecting all $k$ waypoints in the pattern. |
| **Detour Index** | $\alpha = 1.5$ | The empirical campus walkability ratio ($\frac{\text{Walking Distance}}{\text{Euclidean Distance}}$). Real-world campus footpaths, roads, and buildings require walking $\approx 1.5\times$ straight-line distance. |
| **Adjusted Perimeter** | $P_{\text{adj}} = \alpha \cdot P_{\text{tour}}$ | The realistic walking distance (in meters) required for the team to traverse the pattern on campus. |
| **Effort Weight** | $b = 2$ | Damage points awarded per 100 meters of actual walking distance. Rewards physical endurance and geographical territory control. |

---

## 3. Game Design & Engineering Rationale

### A. Preventing Gameplay Exploits
* **Distance Exploitation:** If damage scaled only with distance, a team of two players could repeatedly run between two distant checkpoints (e.g., $DH3 \leftrightarrow R$), ignoring team coordination. The $a \cdot k$ term ensures that multi-player shapes are strictly more valuable.
* **Proximity Exploitation:** If damage scaled only with player count ($k$), teams would group up and cast tiny shapes in a single corner of the campus. The $b \cdot P_{\text{adj}}$ term demands physical territory coverage to maximize damage.

### B. Realistic Campus Geography ($\alpha = 1.5$)
Euclidean (as-the-crow-flies) distances underestimate physical effort on campus because players must navigate around academic blocks, hostels, and sports grounds. Incorporating $\alpha = 1.5$ ensures that teams who hike realistic, difficult routes receive proportional combat reward.

### C. Zero Runtime Complexity ($O(1)$ Hashmap)
Computing convex hulls and solving TSP tours on every QR scan request introduces computational overhead and concurrency locks in backend services. 

By precomputing the entire universe of combinations offline into a static lookup table:
* Lookups are $O(1)$ in Go and TypeScript.
* The game server and client are decoupled from geometric libraries.
* Balancing passes require editing configuration without touching engine code.

---

## 4. Combinatorics Summary

Across the 11 campus checkpoints, the number of distinct playable subsets is:

$$\sum_{k=2}^{6} \binom{11}{k} = 55 + 165 + 330 + 462 + 462 = \mathbf{1,474} \text{ patterns}$$

| $k$ | Shape Classification | Total Combinations | Convex | Concave | Typical Damage Range |
|:---:|---|:---:|:---:|:---:|:---:|
| **2** | Line Segment | 55 | 55 | 0 | 39 – 91 |
| **3** | Triangle | 165 | 165 | 0 | 60 – 113 |
| **4** | Quadrilateral | 330 | 262 | 68 | 84 – 131 |
| **5** | Pentagon | 462 | 235 | 227 | 108 – 148 |
| **6** | Hexagon | 462 | 123 | 339 | 127 – 164 |

