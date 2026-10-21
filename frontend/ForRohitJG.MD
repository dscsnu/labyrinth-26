# For Rohit JG: `/play` runs on test data

**Everything on `/play` currently shows test data, not the backend's live state.** That includes the top bar, the map, the spell book, the scanner's messages and the hidden debug block. Selecting a spell, clearing it and scanning a QR **don't call the backend**: they change a local copy of the test data. That way the whole UI can be built and clicked through even when the QR flow isn't ready, or when the local team is knocked out.

It's all controlled by one flag. Once the QR scanners work end to end, **switch it off and remove the test code** using the steps below.

## The pieces

| What | Where | Is |
|---|---|---|
| `TEST_MODE` | `src/routes/play/+page.svelte` | `true` = test data; `false` = live data |
| `testTeamState` | `src/lib/game/testTeamState.ts` | A full team state, in exactly the format the backend sends |
| `testCasts` | same file | The team's cast log, in the format of `GET /api/teams/me/casts` (newest first) |
| `testView` | `/play` | A `$state` copy of `testTeamState` that the simulated actions change |
| `view` | `/play` | What the page shows: `testView` in test mode, otherwise the live `ts` |
| `shownCasts` | `/play` | What the spell book lists: `testCasts` in test mode, otherwise the live `casts` |

`testTeamState` has the shape of the `message` in a `team_state` event:

```
event: team_state
data: {"type":"scan_added","message":{ …a TeamState… }}
```

That is also the payload of `GET /api/teams/me/state` and the `state` in a scan response. The full field list is the `View` struct in `backend/internal/teamstate/read.go`.

## The test story

The data is a realistic fight in progress, built from the 2026 seed and `config.toml`. Times are relative to page load, so the countdown always runs. **Reloading the page resets everything** to this story.

| When | What happened | Shows up as |
|---|---|---|
| −18 min | The team starts fighting boss 1, Matthew Patel (25 HP) | |
| −8 min | He attacks for 1 | `health: 9` → **x9** in the top bar |
| −7 min | Triangle 2001-2004-2008 (70 dmg) defeats him; the fight with boss 2, Lucas Lee (100 HP), begins | `bosses_defeated: 1`; the triangle in the spell book's Discovered list |
| −2 min | Line 2001-2003 (31 dmg) hits Lucas | `boss.damage: 31` → **69 / 100 HP**; `last_cast`; the line in Discovered |
| −2 min | …so its points go on QR cooldown, and line (every line is `class_2`, the class just cast) on class cooldown | `qr_cooldowns` → **G Block (2001) and Inner gate (2003) dimmed, with a countdown from ~3:00**; `cooldown_shapes: ["line"]` → the Line page shows a **Cooldown** badge and can't be selected |
| −1.5 min | The leader selects the quadrilateral | Both quadrilateral pages show **Selected** and **Clear selection**; the map's book button shows "Image of the spell" |
| −1, −0.5 min | 2002 (Arcade) and 2006 (AIC) are scanned | `stack` → **those two markers wiggle** |
| +3 min | The line's points come off QR cooldown | The two markers go back to normal; scanning them works again |
| +3 min | Lucas attacks | `boss.next_attack_at` → **Attack in 3:00** counting down. Nothing happens at 0:00 in test mode |

Pentagon and hexagon are locked (`locked_shapes`), because the team hasn't defeated 5 bosses.

## What reads what

| On `/play` | Reads |
|---|---|
| Top bar: boss image, name, HP bar | `view.boss` (serial, damage), with name, image and max HP from the real `GET /api/game/bosses` list |
| Top bar: attack countdown | `view.boss.next_attack_at` (hidden while knocked out) |
| Top bar: team health | `view.health` |
| Map: wiggling markers | `view.stack` → `markedWaypoints` → `<MapView active>`. A marker wiggles if its `MAP_POINTS[].waypoint` (`src/lib/game/mapPoints.ts`) is in it |
| Map: dimmed markers with an m:ss countdown | `view.qr_cooldowns` → `<MapView cooldowns>`, timed by `now` (the server clock). An entry whose `until` has passed is ignored, so the marker goes back to normal at 0:00 |
| Map: book button ("Image of the spell") | `view.selection` |
| Spell book: Selected / Select Spell / Clear selection | `view.selection` |
| Spell book: locked spells | `view.locked_shapes` |
| Spell book: **Cooldown** badge, Select Spell disabled with "cast a spell of another class first" | `view.cooldown_shapes` → `<SpellBook cooldownShapes>` |
| Spell book: knocked-out note | `view.knocked_out` |
| Spell book: Discovered lists | `shownCasts` |
| Hidden debug block | `view` and `shownCasts` |

## What's simulated in test mode

These are the functions `testSelect`, `testGiveUp` and `testScan` in `/play`, all marked `TEST MODE`. Each changes `testView` the way the backend would change the real state, and bumps `version.seq`.

| Action | What it does to the test state | Refused (same messages as the backend) when |
|---|---|---|
| **Select Spell** (spell book) | `selection` = that shape, by you, now; `stack` emptied; `poisoned` = false | The team is `knocked_out`, the shape is in `locked_shapes`, or it's in `cooldown_shapes` |
| **Clear selection** (spell book) | `selection` = null; `stack` emptied | Never |
| **Scan a QR** (scanner button) | Adds `{waypoint_id, user_id: you, scanned_at: now, wrong: false}` to `stack`, so **that marker starts wiggling**; also adds it to `waypoints` | It isn't one of the 10 waypoints; no spell is selected; it's already in the stack; it's in `qr_cooldowns` and not yet expired |

To get a real QR to scan locally, open `/qr` as a HELPER or ADMIN and show a waypoint (e.g. `2005`).

**Limits of the simulation** (the real backend does all of these):
- The QR's signature and freshness aren't checked. The waypoint ID is just read from the token (`"<waypoint>:<seconds>.<signature>"`).
- A scan is never marked **wrong**, and scanning a full pattern **never casts a spell**. That needs the backend's pattern data, so the boss's HP, the casts and the cooldowns don't change.
- **No boss attacks.** The countdown reaches 0:00 and stays there.
- **Cooldowns only run out.** QR cooldowns end by time on the page. Since casting isn't simulated, no new QR or class cooldowns start, and the class cooldown on Line never ends (on the backend it ends when the team casts a spell of another class).
- Live `team_state` events still arrive and update `ts` in the background, but nothing shows them, and their pop-up messages (boss defeated, attacked, knocked out, revived) are suppressed.

## What stays live, and why

- **`isLeader`**, which decides whether you see Select Spell / Clear selection, comes from the live `team` event (the team roster). That isn't part of `team_state`. In a one-person team you're the leader.
- **Game content** (`game.shapes` for the spell book's pages, `bosses` for names, images and HP) comes from `GET /api/game/shapes` and `GET /api/game/bosses`. It's the same for every team and doesn't change during a game.
- **The event stream, `ts` and the cast log** keep running underneath, ready for the switch.

## Going live (once the QR scanners work)

1. In `src/routes/play/+page.svelte`, set `const TEST_MODE = false;`. `/play` then shows the live `ts` and cast log, and the actions call the backend again (`PUT`/`DELETE /api/teams/me/state/selection`, `POST /api/teams/me/state/scans`).
2. Check it end to end, with a staff phone on `/qr` showing a waypoint's QR and the team on `/play`:
   - **Scan:** the leader selects a spell in the spell book and someone scans the QR. That waypoint's marker starts wiggling on **every teammate's phone** (they get the `team_state` event). It stops when the stack empties: a new selection, a restart, clearing the selection, or completing the spell.
   - **Cast:** completing a spell adds it to the spell book's Discovered list and drops the boss's HP bar. Defeating the boss switches the top bar to the next one.
   - **Boss attack:** when the countdown reaches 0:00, the backend attacks. The team health goes down (e.g. x10 → x9) and the countdown restarts.
   - **Cooldowns:** after a cast, its points dim with a 5:00 countdown on every phone, and that spell's class shows Cooldown in the spell book. The backend sends `qr_cooldown_ended` when a point reopens. An admin's "reset cooldowns" (`cooldowns_reset`) clears both at once.
3. Remove the test code:
   - in `/play`: `TEST_MODE`, `testView`, `testChange`, `testSelect`, `testGiveUp`, `testScan`, the `if (TEST_MODE) …` lines, and the test import;
   - replace `view` with `ts` and `shownCasts` with `casts` (or keep `let view = $derived(ts)`);
   - delete `src/lib/game/testTeamState.ts` (nothing else uses it).

**If the local team is knocked out** when testing live: boss attacks keep coming every 10 minutes while a team is in a game, even with nobody on `/play`. A team that started about 100 minutes ago without beating boss 1 will have 0 health. Make yourself an admin in the local database (from `backend/`):

```sh
docker compose exec postgres psql -U labyrinth -c "UPDATE userprofile SET role = 'ADMIN' WHERE email = '<your email>';"
```

Then open `/admin` and click **revive (full health)** on your team.

## Useful `team_state` fields for later

| Field | Use |
|---|---|
| `stack[]` `{waypoint_id, user_id, scanned_at, wrong}` | Points scanned in this attempt (what wiggles now). `wrong: true` means that scan spoiled the attempt, so you could show it in red. |
| `knocked_out` | Health 0: the team can't select or scan until an organiser revives it. |
| `waypoints[]` | Every point the team has ever had a scan accepted at. |
| `last_cast` | The latest spell cast: its `pattern` (waypoint IDs), damage, boss hit. |
| `version` `{epoch, seq}` | Ordering: keep a copy only if it's newer (`isNewer` in `src/lib/types/Live.type.ts`). |
