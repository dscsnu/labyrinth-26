# Activity: the admin live view's feed

`activity` owns the feed behind `/admin/live`: every change to any team's game state, as it happens, for organisers. Config values appear with their key: **1 s** (`activity.publish_every`), **200** (`activity.batch_max`), **200** (`activity.recent`).

## Contents
1. [Concepts](#concepts)
2. [The flow](#the-flow)
3. [How each file works](#how-each-file-works)
4. [Data model: memory](#data-model-memory)
5. [Data model: Postgres](#data-model-postgres)
6. [Memory ↔ Postgres](#memory--postgres)
7. [Walkthroughs](#walkthroughs)
8. [The write-through](#the-write-through)
9. [API](#api)
10. [Rules and cases](#rules-and-cases)
11. [Concurrency and locking](#concurrency-and-locking)
12. [Failure and recovery](#failure-and-recovery)
13. [Limits and how to change them](#limits-and-how-to-change-them)
14. [Not decided yet](#not-decided-yet)
15. [Tests](#tests)

## Concepts
| Term | Meaning |
|---|---|
| **Entry** | One change to one team's game state, as the live view shows it: when, which team, the cause (the `team_state` event type), who acted, and the facts for that cause (shape, waypoint, damage, boss, health, powerup, quiz answer). |
| **Queue** | The entries since the last flush: a ring of `activity.batch_max`. Past it the oldest are overwritten and counted as **dropped**. |
| **Batch** | What a flush sends: the queued entries, oldest first, and the dropped count, as one `activity` event of type `batch`. |
| **Recent** | The last `activity.recent` flushed entries, kept for a view that opens later (its `snapshot`). |
| **Watcher** | An admin's stream opened with `GET /api/events?admin=1` (the `/admin/live` page). Only watchers receive batches and the full leaderboard, and only on that stream. |

## The flow
1. **Record.** Every time team state publishes a team's state (`teamstate.Store.publish`, once per change: a selection, a scan, a cast, an attack, a revive, a powerup, a quiz answer…), it calls `Rules.OnPublish`, which `server.Build` points at `Feed.Record`. Record builds an `Entry` from the cause and the state just sent, names the member from teams' memory, and queues it. `snapshot` and `resync` are resends, not changes, and are ignored.
2. **Flush.** `Run` calls `Flush` every `activity.publish_every`. Flush moves the queue to the recent ring and, if anything was queued or dropped, and anyone watches, sends one `activity` batch to every watcher.
3. **Watch.** `server.events` handles `?admin=1`: only then it reads the caller's role from Postgres (as `RequireAdmin` does). For an `ADMIN` it calls `Watch(sub)` with that stream's subscription (stopping when the stream ends) and appends the `activity` snapshot (recent) and the full leaderboard snapshot. Anyone else gets the normal stream.
4. **Full leaderboard.** The leaderboard use case sends the full ranking to watchers through `SendAdmins` (see its README).

**Why batched.** At the load test's peak (~5,300 scans/s) an event per change would flood an admin's browser and cost an encode per change. One batch a second costs one encode of at most `activity.batch_max` entries, whatever the rate.

**Why a ring.** The queue can't grow with the scan rate: a spike drops the oldest entries (and says how many) instead of using memory.

**Why the role is read only on `?admin=1`.** After a restart every phone reconnects at once; reading every stream's role would add a query each. Players never ask, so they cost nothing.

**Why one stream, not the user.** An admin may also have `/play` open. `live.Subscription.Send` delivers to the `?admin=1` stream alone.

## How each file works
- `doc.go`: the package comment.
- `read.go`: `Teams` (what it reads from teams: `ByID`), `Entry`, `Batch`, `Rules` (`PublishEvery`, `BatchMax`, `Recent`, and `Now`, the clock, nil for `time.Now`), and `Snapshot` (the recent entries).
- `write.go`: `Record` (build and queue an entry; names the powerup granted or activated by comparing the team's held powerups with the last ones seen), `Watch` / `Watching`, `SendAdmins`, and `Flush`.
- `store.go`: the layout, `Feed`, `New`, `Batched` (the `batch` type), `Run`, `SnapshotEvent`, and `ring` (the fixed-size queue).
- No `api.go` (no routes: the feed rides `GET /api/events`) and no `db.go` (nothing stored).

## Data model: memory
| Field | Go type | Meaning |
|---|---|---|
| `pending` | `ring` of `activity.batch_max` | entries since the last flush, oldest first |
| `dropped` | `int` | entries overwritten in `pending` since the last flush |
| `recent` | `ring` of `activity.recent` | the latest flushed entries |
| `held` | `map[team ID][]string` | each team's held powerups as last seen, to name the one granted or activated |
| `watchers` | `map[*live.Subscription]struct{}` | the watching streams |

One `sync.Mutex` guards all of it. Fixed size apart from `held` (one small slice per team that has published).

## Data model: Postgres
None: activity reads and writes no tables.

## Memory ↔ Postgres
Nothing is persisted. A restart starts with an empty feed and no watchers (their streams reconnect and watch again); the changes before it aren't replayed. Acceptable: the feed is a view of what happens now, and the game state it describes is team state's, which is persisted.

## Walkthroughs
Team `482913` "Scanners" (Asha, Ravi), `activity.batch_max` 200, one admin watching.

| Step | Change | Queue after | Sent |
|---|---|---|---|
| 1 | Asha selects a triangle | `shape_selected` (shape `triangle`, by Asha) | |
| 2 | Ravi scans `2003` | + `scan_added` (waypoint `2003`, by Ravi) | |
| 3 | Asha's scan completes it, defeating boss 1 | + `boss_defeated` (shape `triangle`, dealt 68, boss 1, by Asha) | |
| 4 | flush | empty; recent holds the three | `activity: batch {entries: [3], dropped: 0}` |
| 5 | boss 2 attacks | `boss_attacked` (health 8, boss 2, by null) | |
| 6 | 450 scans across teams in a second | the newest 200; dropped 251 | |
| 7 | flush | empty | one batch of 200, `dropped: 251` |

## The write-through
Doesn't apply: nothing is persisted.

## API
No routes of its own. On `GET /api/events?admin=1` for an `ADMIN` (`internal/server/README.md`, "The event stream"):

```
event: activity
data: {"type":"batch","message":{"entries":[{"at":"2026-10-25T12:00:01.204Z","team_id":"482913","team":"Scanners","type":"spell_cast","by":"Asha","shape":"triangle","dealt":68,"boss":1}],"dropped":0}}
```

`entries` is oldest first. `by` is null for what the server does by itself (attacks, cooldowns ending). The fields an entry has depend on its type:

| Type | Fields |
|---|---|
| `shape_selected` | `shape`, `by` |
| `scan_added`, `wrong_scan` | `waypoint`, `by` |
| `spell_cast`, `boss_defeated` | `shape`, `dealt`, `boss`, `by` |
| `boss_attacked`, `knocked_out`, `revived` | `health`, `boss` (null after the last) |
| `powerup_granted`, `powerup_activated` | `powerup` |
| `qte_answered` | `correct`, `by` |
| others (`game_started`, `selection_cleared`, `qr_cooldown_ended`, `cooldowns_reset`, `powerup_ended`) | none |

## Rules and cases
**Record**

| Case | Result |
|---|---|
| type `snapshot` or `resync` | ignored |
| queue full | the oldest entry is overwritten; `dropped` + 1 |
| member not in the roster (removed since) | `by` null |
| `powerup_granted` / `powerup_activated` | the kind held now and not before / held before and not now; "" if the feed hadn't seen the team's powerups |

**Flush**

| Case | Result |
|---|---|
| nothing queued or dropped | nothing sent; returns false |
| something queued, nobody watching | the queue moves to recent; nothing sent |
| something queued, watchers | one `activity: batch` to each watching stream |

**`?admin=1`**: the caller's role is `ADMIN` in Postgres → watching, with the two admin snapshots; otherwise (a player, a helper, a database error) the normal stream. A demoted admin keeps watching until that stream reconnects.

## Concurrency and locking
`Record` runs inside team state's publish, holding that team's entry lock. It reads teams' memory (`ByID`, as publish itself does), then takes the feed's mutex for a few slice writes: no I/O, no encoding, so a team's writes aren't held up. Lock order: team state's entry, then the feed's mutex; nothing takes them the other way. `Flush` encodes and sends outside the mutex. Entries from concurrent publishes of different teams are queued in the order they take the mutex.

## Failure and recovery
An encoding error drops that batch (logged). A slow admin stream is dropped by the hub like any other and reconnects with a fresh snapshot. A restart loses the queue and recent entries (see Memory ↔ Postgres).

## Limits and how to change them
| Limit | Where | Change |
|---|---|---|
| Flush interval: **1 s** | `activity.publish_every` (100 ms–1 min) | Edit, restart |
| Entries per batch: **200** | `activity.batch_max` (1–10,000) | Edit, restart |
| Entries a new view starts with: **200** | `activity.recent` (0–10,000) | Edit, restart |

## Not decided yet
- **Role read once.** A demoted admin keeps the feed until their stream reconnects.
- **No history.** Changes before the view opened (beyond `activity.recent`) or before a restart aren't available.

## Tests
| Test | File | Covers |
|---|---|---|
| `TestEntriesByType` | `tests/feed_test.go` | each type's fields; `by` by name, null for an attack; the powerup granted then activated named from held; snapshot and resync ignored |
| `TestBatchingIsBounded` | `tests/feed_test.go` | an empty flush sends nothing; past `batch_max` the oldest dropped and counted; `recent` keeps the last N; the snapshot |
| `TestOnlyWatchersReceive` | `tests/feed_test.go` | batches and `SendAdmins` reach the watching stream, not another; stop unwatches; nobody watching sends nothing |
| `TestOnPublishSeesEveryPublish` | `internal/teamstate/tests/onpublish_test.go` | team state calls the hook once per publish, with the cause and state |
| `TestFullRankingForAdmins` | `internal/leaderboard/tests/leaderboard_test.go` | the full ranking to admins, past `leaderboard.size`, only on change |
| `TestSubscriptionSendReachesOnlyThatStream` | `internal/platform/live/tests/hub_test.go` | one stream, not the user's others; nothing once closed |
| `TestAdminLiveView` | `internal/server/tests/activity_test.go` | over HTTP: an admin's `?admin=1` stream's snapshots, a batch after a cast (selection, scans, cast, by whom), the full leaderboard; an admin's plain stream and a player's `?admin=1` get nothing |
