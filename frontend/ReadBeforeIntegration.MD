# Read before integration

How `/play`'s **top banner** (the current boss's image, name and HP), the **spell book** (the book button) and the **map markers** get their data, and what to keep in mind when building on them.

Both are fed by the same two mechanisms, and neither makes a request of its own:

1. **The page load**, once, in `src/routes/play/+page.server.ts`.
2. **The live event stream**, `/api/events`, opened in `src/routes/play/+page.svelte`'s `onMount`, which keeps the team's state current on every teammate's phone.

## Contents
1. [The page load](#1-the-page-load)
2. [Live updates](#2-live-updates)
3. [The top banner: boss name, HP and asset](#3-the-top-banner-boss-name-hp-and-asset)
4. [The spell book: the spells](#4-the-spell-book-the-spells)
5. [The map markers](#5-the-map-markers)
6. [What the backend doesn't send](#6-what-the-backend-doesnt-send)
7. [Rules for integrating](#7-rules-for-integrating)
8. [Troubleshooting](#8-troubleshooting)

---

## 1. The page load

`+page.server.ts`'s `load` runs on the SvelteKit server and calls the backend in parallel with the session token (`backendFetch` in `src/lib/server/backend.ts`). Each response is checked with its zod schema from `src/lib/types/Game.type.ts`.

| Backend route | Becomes | Schema | Changes? |
|---|---|---|---|
| `GET /api/game/bosses` | `data.bosses` | `BossListSchema` | Never while the backend runs. Loaded once. |
| `GET /api/game/shapes` | `data.game` | `GameSchema` | Never while the backend runs. Loaded once. |
| `GET /api/teams/me/state` | `data.state` | `TeamStateSchema` | Every scan, selection and cast. Kept live (§2). |
| `GET /api/teams/me/casts` | `data.casts` | `CastLogSchema` | On every cast. Kept live (§2). |

A `404` from the state route means the player isn't in a team, so the load redirects to `/team`.

## 2. Live updates

The page keeps two pieces of live data:

```ts
let live = $state<TeamState | undefined>(undefined);
let ts = $derived(live ?? data.state);   // the team state everything reads
let casts = $state<Cast[]>(...data.casts); // the cast log, newest first
```

New copies of the team state arrive two ways, and **both go through `observe(s)`**:

- **The event stream.** The browser opens `new EventSource("/api/events")`. In dev, `vite.config.ts` proxies it to `BACKEND_URL`; in production, the reverse proxy does. Whenever anyone on the team scans, selects or casts, the backend sends a `team_state` event to **every member's** stream (`backend/internal/teamstate/store.go`, `publish`).
- **Write responses.** `/play/scan` and `/play/selection` return the new state, so the player who acted sees it at once.

Every event's data is an envelope `{ type, message }`, read with `envelope(TeamStateSchema)` from `src/lib/types/Live.type.ts`. `message` is the **whole** state. `type` says why it was sent (`spell_cast`, `boss_defeated`, `scan_added`, …) and is used only for one-off notices.

`observe(s)` does three things:
1. **Keeps it only if it's newer:** `isNewer(ts.version, s.version)`. A response and its event can arrive in either order, and the older one must not overwrite the newer.
2. **Prepends `s.last_cast`** to `casts` if the log doesn't have it yet (matched by `cast_at`).
3. **Re-fetches the whole cast log** through `GET /play/casts` (a proxy to `/api/teams/me/casts`) if `s.cast_count > casts.length`, meaning casts were missed while disconnected.

On every connect or reconnect, the stream first sends the current state (type `snapshot`), so a phone that dropped off catches up immediately.

## 3. The top banner: boss name, HP and asset

The markup is the `<!-- Top bar -->` block in `src/routes/play/+page.svelte`.

### Where each value comes from

The boss's details and the team's progress arrive separately and are **joined on the client by serial**:

```ts
// +page.svelte
let boss = $derived(ts.boss ? bosses.find((b) => b.serial === ts.boss!.serial) ?? null : null);
```

| Shown | Value | Source |
|---|---|---|
| Image | `/{boss.asset}`, e.g. `/bosses/1.png` | `boss.asset` from `GET /api/game/bosses` (static) |
| Name | `boss.name` | `GET /api/game/bosses` (static) |
| HP bar and text | `max(0, boss.hp - ts.boss.damage)` / `boss.hp` | `boss.hp` (static) − `ts.boss.damage` (live team state) |
| Which boss | `ts.boss.serial` | Live team state |

The two payloads:

```jsonc
// GET /api/game/bosses → data.bosses (BossSchema[])
[{ "serial": 1, "name": "Matthew Patel", "asset": "bosses/1.png", "description": "…", "hp": 200 }, …]

// team_state (and GET /api/teams/me/state) → ts, the boss-related fields only
{ "bosses_defeated": 0, "boss": { "serial": 1, "damage": 107 }, "damage_multiplier": 1, … }
```

`ts.boss` is `null` once every boss is defeated. The banner then says "Every boss defeated!", or "No bosses loaded." if `bosses` is empty.

### How it stays live

Nothing in the banner fetches anything. When a teammate's cast lands, the backend sends the new `team_state` to every member. `observe` replaces `ts`, and `boss`, the HP and the image all recompute. When a boss is defeated, `ts.boss.serial` moves on, so the name, HP and image switch to the next boss on every phone. A `team_state` of type `boss_defeated` also shows a "<name> defeated!" notice.

### The asset

`asset` is a **key, not a URL**. The frontend puts `/` in front of it, and SvelteKit serves it from `static/`:

```
static/bosses/1.png   →   /bosses/1.png   (asset "bosses/1.png")
```

- The images live in `static/`, not `src/lib/assets/`, because the path comes from backend data at runtime. Vite can only process images that the code imports by a path it knows at build time.
- A missing image hides itself (`onerror`), leaving the name and HP. The `<img>` is wrapped in `{#key boss.serial}` so that one boss's missing image doesn't hide the next boss's image.
- To change an image's file name or format, change the `asset` values in `backend/internal/platform/postgres/seeds/dev_2025_bosses.sql`. Then stop the backend and run `go run . seed` from `backend/`.

## 4. The spell book: the spells

The component is `src/lib/components/SpellBook.svelte`. `+page.svelte` mounts it with:

```svelte
<SpellBook bind:open={bookOpen} shapes={game.shapes} {casts} {selected} />
```

| Prop | Source | Live? |
|---|---|---|
| `shapes` | `data.game.shapes` from `GET /api/game/shapes` | Static |
| `casts` | The page's `casts` (§2): loaded from `GET /api/teams/me/casts`, then each new `last_cast` prepended | Yes |
| `selected` | `ts.selection?.shape` | Yes |

### What a "spell" is

In the backend, a **pattern** is a set of waypoints that forms a **shape**. Completing a pattern **casts** its spell, and a spell's only data is its **damage**. The book has **one page per shape**.

### Each page

| Shown | Field | Source |
|---|---|---|
| Name | `shape.name` | `/api/game/shapes` |
| Points | `shape.points` (allowed pattern sizes, e.g. `[3, 4]`) | `/api/game/shapes` |
| Damage range | `shape.damage.min` to `shape.damage.max` (one number if they're equal) | `/api/game/shapes` |
| Found | discovered count / `shape.patterns` | count from `casts`; total from `/api/game/shapes` |
| Discovered list | each pattern's waypoints, `damage`, and the number of times cast | `casts` |
| "Selected" tag | `shape.id === selected` | Team state |

```jsonc
// GET /api/game/shapes → data.game (GameSchema)
{ "shapes": [{ "id": "square", "name": "Square", "patterns": 5, "points": [4], "damage": { "min": 60, "max": 131 } }],
  "max_pattern_points": 6 }

// One cast (CastSchema): an entry of GET /api/teams/me/casts, or team_state.last_cast
{ "pattern": ["1001", "1004", "1007", "1010"], "shape": "square", "cast_by": "<user id>",
  "cast_at": "2026-10-21T10:15:00Z", "already_completed": false,
  "damage": 107, "dealt": 107, "boss": 1, "defeated_boss": false }
```

`discovered` (a `$derived` in the component) groups `casts` by `shape`, then by pattern (`pattern.join("-")`). Repeat casts increase a pattern's count instead of adding a new row. Because `casts` is live, a teammate's cast shows up in every open book.

### Behaviour

- The book opens on the team's selected shape, or on the page it was left on. It only turns to the selected shape **on opening**, so a teammate's selection never turns someone's page while they're reading.
- The `<` and `>` buttons at the bottom (and the left and right arrow keys) turn pages. Close is in the middle; Escape or a tap on the dimmed map also closes the book.

## 5. The map markers

The arrows on the map. They're **frontend-only for now**: the positions belong to the map image, and they aren't connected to the backend yet.

### Where they're defined

Everything is in **`src/lib/game/mapPoints.ts`**. To move, rename, add or remove a marker, edit that file only.

```ts
export const MAP_SIZE = { width: 1168, height: 1346 };   // map.webp's pixels

export const MAP_POINTS: MapPoint[] = [
    { name: "G Block", x: 517, y: 222, waypoint: null },
    …
];
```

| Field | Meaning |
|---|---|
| `name` | The place's name. Read by screen readers; must be unique, because it keys the marker. |
| `x`, `y` | Where the arrow's tip touches, in `map.webp`'s own pixels, from `(0, 0)` at the top-left. To find them, hover the spot in an image editor and read the cursor's position. |
| `waypoint` | The backend waypoint (the 4-digit ID its QR code prints). `null` until the backend's waypoints are final. |

### How they're drawn

- `+page.svelte` passes the points in: `<MapView bind:level={zoomLevel} markers={MAP_POINTS} />`.
- In `src/lib/components/MapView.svelte`, the map image and the markers share one box. The box is as tall as the viewport × zoom, with `map.webp`'s shape (`aspect-ratio`). Each marker is placed at `x / width` and `y / height` **as percentages** of the box, so zooming and panning carry the markers along.
- The sprite is `src/lib/assets/MapIcons/down-arrow.png` (16×16, shown at its own size, 16×16 screen pixels). Its tip is the bottom-centre of the image (columns 7–8 of the bottom row). The marker is shifted by `-translate-x-1/2 -translate-y-full`, so that point lands exactly on `(x, y)`.
- Markers stay the same size at every zoom level, and taps pass through them to the map (`pointer-events-none`), so dragging still works.
- If `map.webp` is replaced with an image of a different size, update `MAP_SIZE` and re-pick every point.

### Integrating with the backend

1. When the backend's 2026 waypoints are final, set each point's `waypoint` to its ID, e.g. `{ name: "G Block", x: 517, y: 222, waypoint: "1005" }`. Don't change the coordinates; they belong to the map, not the backend.
2. Connect markers to live team data by matching `waypoint` against the team state, in `+page.svelte` (never inside `MapView`), and pass the result as a prop:
   ```ts
   const state = (id: string | null) =>
       id && ts.stack.some((s) => s.waypoint_id === id) ? "active"   // in the current attempt
     : id && ts.waypoints.includes(id) ? "visited"                     // scanned before
     : "unvisited";
   ```
   `ts.waypoints` and `ts.stack` are live (§2), so markers would update on every teammate's phone with no new requests.
3. The backend's waypoint list and `MAP_POINTS` should hold the same IDs. A point with a `waypoint` the backend doesn't know never lights up; a waypoint with no point has no marker.

## 6. What the backend doesn't send

These are deliberate. Don't build features that assume them:

- **Undiscovered patterns.** The waypoints in each pattern, and each pattern's exact damage, are hidden until the team casts it. The book can show only the count of patterns still to find.
- **Waypoint coordinates and names.** The game routes don't expose them. The map markers' positions and names are in `src/lib/game/mapPoints.ts` (§5).
- **Spell names, effects or icons.** A spell is only `pattern.damage`. Anything more needs a backend change (a column or table, plus the API).
- **The cast log on the stream.** Only the newest cast (`last_cast`) is pushed. The full log comes from `/api/teams/me/casts`.
- **The boss list on the stream.** It's static, so it's loaded once.
- **Team health.** The top bar's "Team Health" (top right, next to `src/lib/assets/icons/scott-face.png`) is a placeholder: `teamHealth` in `+page.svelte` is `null` and shows "?". Once the backend adds the value to `team_state`, derive `teamHealth` from `ts` there and it updates live like everything else.

## 7. Rules for integrating

- **Read from `ts`, `bosses`, `game` and `casts`; don't fetch.** Anything derived from them updates live for the whole team at no extra cost. A component that fetches the boss or state itself would go stale.
- **Send every new team state through `observe`.** Never assign `live` directly: that skips the version check and the cast-log merge.
- **Use `type` only for one-off moments** (notices, animations such as a boss's `hit` on `spell_cast` or its death on `boss_defeated`), not to work out state. The state is always the whole `message`.
- **Pass data into components as props** (like `SpellBook`) instead of having them load it, so there's one source of truth.
- **Join bosses by `serial`**, not by array index.
- **Clamp HP at 0** when displaying it.

## 8. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Banner says "No bosses loaded." | The `boss` table is empty (the database was seeded before the bosses migration). `GET /api/game/bosses` returns `[]`. | Stop the backend, run `go run . seed` in `backend/` (safe to re-run), and start it again. |
| Name and HP show but no image | No file at `static/<asset>` | Add `static/bosses/<serial>.png`, or fix `asset` in the seed. |
| A phone loads `/play` but never updates live | Dev mode (`DEV_TOKEN`) gives the **page load** a session but sets no `labyrinth_token` cookie. `/api/events` goes straight to the backend through the proxy, which needs that cookie, so the stream isn't signed in. | Sign in on that phone, or have dev mode set the cookie. This is a known gap, not fixed yet. |
| The spell book's "Found" count looks low | Casts from before migration `0007` aren't in the log. | Expected for old data. New casts are counted. |
