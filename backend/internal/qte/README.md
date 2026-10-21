# qte

Quick-time events: game-wide events an admin starts that every player sees at once, a **quiz** being the first kind. A team that answers a quiz right earns the QTE's powerup. One QTE runs at a time.

Config values appear with their key: **30 s** (`qte.answer_window`), **5 min** (`qte.max_answer_window`).

## Contents
1. [Concepts](#concepts)
2. [The flow](#the-flow)
3. [How each file works](#how-each-file-works)
4. [Data model: memory](#data-model-memory)
5. [Data model: Postgres](#data-model-postgres)
6. [Memory ↔ Postgres](#memory--postgres)
7. [The write-through](#the-write-through)
8. [API](#api)
9. [Rules and cases](#rules-and-cases)
10. [Concurrency and locking](#concurrency-and-locking)
11. [Failure and recovery](#failure-and-recovery)
12. [Limits and how to change them](#limits-and-how-to-change-them)
13. [Not decided yet](#not-decided-yet)
14. [Tests](#tests)

## Concepts
| Term | Meaning |
|---|---|
| **QTE** | A quick-time event (`QTE`, table `qte`): a kind, the powerup kind a correct answer earns, the kind's content, an optional window, and when it ran. Runs at most once. |
| **Kind** | What a QTE is. Only `quiz` (`KindQuiz`) for now. Each kind has its own columns, its own CHECK in the table, and its own key in the JSON (`quiz`), so a new kind adds those, not plumbing. |
| **Quiz** | A question, at least **2** options (`MinOptions`, a correctness bound), and the index of the right one. Options are numbered from 0. |
| **Status** | Derived from the clock, never stored: `draft` (never started), `open` (started, now before `ends_at`), `closed`. |
| **Window** | How long a QTE stays open once started: its own `window_s` (1 s to **5 min**, `qte.max_answer_window`), else **30 s** (`qte.answer_window`). Starting stores `ends_at`, which then rules: a config change never moves a running QTE. |
| **Answer** | A team's answer is **teamstate's**: one per team per QTE, right or wrong at once, the powerup granted in the same transaction (teamstate README, "QTE answers"). teamstate reads QTEs through `ByID`. |
| **Public view** | A QTE as players see it (`View`): without its answer while open; with it (`correct_option`) only in the closing event. Built in one place, `publicView`. |

## The flow
1. **Write.** An admin writes a quiz (`POST /api/admin/qtes`, the admin use case calling `Create`): a draft to launch later, or started at once. Placeholder drafts are also seeded (`seeds/2026_qtes.sql`, run by `Seed2026`: ids 1–4, one per powerup, which a rerun refreshes only while still unstarted seeded drafts).
2. **Start.** Starting a draft (`Start`) or creating one started sets `started_at` = now and `ends_at` = now + its window. Every stream gets the game-wide `qte` event, type `started`: the quiz without its answer.
3. **Answer.** Teams answer through teamstate (`POST /api/teams/me/state/qte/{qte_id}/answer`).
4. **Close.** At `ends_at` the server closes it with nobody acting: the timer broadcasts `closed`, the quiz with its answer revealed. An admin may end it sooner (`End`: `ends_at` = now, same event). From then on the resource is `null` (on connect and `GET /api/qte`) until the next QTE starts.

**Why closing writes nothing.** A QTE is closed because the clock passed its stored `ends_at`, which every read and every answer check already compares against. The timer only announces it. So a late timer can never let an answer in, and `Load` reproduces the state exactly (AGENTS.md §6, a time-driven change derived from what's stored).

**Why answers are teamstate's.** A team's result from a game-wide event is a per-team result, so it lives in `team_state` (AGENTS.md §17), and recording the answer and granting the powerup must be one transaction.

## How each file works
### `doc.go`
The package comment.

### `store.go`
- **Layout:** one store, not one entry per key (QTEs run one at a time, and there are tens of them): `mu` for writers and the timer; `all`, an `atomic.Pointer` to every QTE (newest first, never modified: a write stores a new slice); `stopTimer`, `armedFor` (the QTE the timer will close) and `closed`, guarded by `mu`.
- **`Clock`** (`Now`, `AfterFunc`) and **`systemClock`**, as teamstate's: tests pass a fake through `Rules.Clock`.
- **`Rules`**: `AnswerWindow`, `MaxAnswerWindow`, `Powerups` (teamstate's kinds, from `teamstate.PowerupIDs` in `server.Build`, so qte needn't import teamstate), `WriteTimeout`, `Clock`.
- **`Load`**: `selectQTEs`, refusing a stored QTE of a kind or powerup the code doesn't know (`check`), then arms the timer if one is open.
- **`Close`**: stops the timer; none arms or acts afterwards. Registered on shutdown (`main.go`) and called by the tests' `restart`.
- **`persist`**: a write detached from the request, bounded by **5 s** (`storage.write_timeout`).
- **`reconcile`**: after a failed write, reloads every QTE (Postgres wins), broadcasts the open one as `resync`, re-arms the timer.
- **`schedule`**: arms one `AfterFunc` for the open QTE's `ends_at` (`armedFor` = its ID), replacing any pending one.
- **`due(id)`**: the timer's callback. Nothing if `armedFor` isn't `id` (it was ended or replaced, or the store closed: `Close` clears `armedFor` and a closed store never arms); re-arms if it fired early; else broadcasts `closed` with the answer. Writes nothing.
- **`broadcast`**: `live.NewEvent("qte", type, view)` then `Hub.Broadcast`, under `mu`, so events go out in order.
- **Types** `Started`, `Closed` (and `live.Snapshot`, `live.Resync`).

### `read.go`
- `QTE` (full, with the answer; admins only) and `Quiz`; `Status(now)`.
- `ByID` (teamstate's interface), `List` (newest first), `open(now)`: lock-free.
- `View`, `QuizView` and `publicView(q, closed)`: the one player-facing builder; `correct_option` is set only when `closed`.
- `Public` (`GET /api/qte`: the open QTE or nil) and `Snapshot` (the `qte` event for a new stream: the open QTE or null).

### `write.go`
- `Draft` and `validate` (kind, powerup, question, options, correct option, window, in that order).
- `Create`, `Start`, `End`: the write-through steps below. `replace` swaps one QTE in a new slice.

### `db.go`
- `selectQTEs`: every row, newest first.
- `insertQTE`: one `INSERT … RETURNING id, created_at`, started or a draft.
- `updateQTEStarted`: `UPDATE … SET started_at, ends_at WHERE id AND started_at IS NULL`, exactly one row.
- `updateQTEEnded`: `UPDATE … SET ends_at WHERE id AND started_at IS NOT NULL AND ends_at > now`, exactly one row.
- `mapOverlap`: the exclusion constraint's violation (`23P01`, `qte_tstzrange_excl`) becomes `errOverlap`.

### `api.go`
`GET /api/qte`. The admin routes are the admin use case's; answering is teamstate's.

## Data model: memory
```
Store
  all        atomic.Pointer[[]QTE]   every QTE, newest first; immutable
  mu         sync.Mutex              writers, the timer, broadcasts
  stopTimer  func() bool             the pending close timer (mu)
  armedFor   int64                   the QTE it closes; 0 if none (mu)
  closed     bool                    set by Close (mu)

QTE
  ID         int64                   qte.id
  Kind       string                  "quiz"
  Powerup    string                  the powerup kind a right answer earns
  Quiz       *Quiz                   Question, Options []string, CorrectOption int
  WindowS    *int                    its own window in seconds; nil: qte.answer_window
  CreatedBy  *uuid.UUID              the admin; nil: seeded
  CreatedAt  time.Time
  StartedAt  *time.Time              nil: a draft
  EndsAt     *time.Time              when answers stop
```
Tens of QTEs a day: the whole list is copied on a write, which is cheap at that size.

## Data model: Postgres
| Table | Migration | Columns | Constraints and what they back |
|---|---|---|---|
| `qte` | `0015` | `id` BIGINT identity (`BY DEFAULT`, so seeds can use fixed ids), `kind`, `powerup`, `question`, `options` TEXT[], `correct_option`, `window_s`, `created_by` → `userprofile` (`ON DELETE SET NULL`), `created_at`, `started_at`, `ends_at` | `kind IN ('quiz')`; `powerup` one of the four kinds; a quiz has a question, ≥ 2 non-null options and a right one in range; `window_s > 0`; `started_at` and `ends_at` both null or both set, `ends_at ≥ started_at`. **An exclusion constraint on `tstzrange(started_at, ends_at, '[)')`**: no two runs overlap, the database's backstop for "one at a time". |

`team_qte_answer` (same migration) is teamstate's.

## Memory ↔ Postgres
| Postgres | Memory |
|---|---|
| every `qte` row, newest first | `all` |
| (none: derived from `started_at`, `ends_at` and the clock) | each QTE's status |
| (none: memory-only) | the close timer: re-armed by `Load` for an open QTE |

Nothing else is held only in memory: a restart reproduces every QTE exactly, and an open one keeps its `ends_at` (`TestRestart`). One that closed while the backend was down loads closed, with no `closed` event: no stream survives a restart, and reconnecting phones get `null`.

## The write-through
| Write | Steps |
|---|---|
| `Create` | `2. validate` (the draft) before `1. lock`; `2. validate` (when starting: none open); `4. persist` before `3. build` (`insertQTE`: Postgres sets `id`, `created_at`); `5. apply`; `6. publish` `started` and arm the timer, when started |
| `Start` | `1. lock`; `2. validate`; `3. build` (`started_at`, `ends_at`); `4. persist` (`updateQTEStarted`); `5. apply`; `6. publish` `started`, arm the timer |
| `End` | `1. lock`; `2. validate`; `3. build` (`ends_at` = now); `4. persist` (`updateQTEEnded`); `5. apply`; `6. publish` `closed` (the answer), drop the timer |
| the timer (`due`) | `4. persist: none, memory-only`: it only broadcasts `closed` |

On a failed persist `reconcile` reloads everything and broadcasts `resync`; an overlap refused by the database is reported as `ErrQTEOpen`. Every refusal returns before step 4: nothing written or published.

## API
| Method | Path | Response |
|---|---|---|
| GET | `/api/qte` | the open QTE as players see it, or `null` |

```json
{"id": 7, "kind": "quiz", "status": "open", "powerup": "heart",
 "started_at": "2026-10-24T11:00:00Z", "ends_at": "2026-10-24T11:00:30Z",
 "quiz": {"question": "Which building has the clock tower?", "options": ["A", "B", "C", "D"], "correct_option": null}}
```

**The `qte` event** (game scope: every stream), its data the envelope `{"type", "message"}`:

| `type` | When | `message` |
|---|---|---|
| `snapshot` | on connect, after `team_state` and before `leaderboard` | the open QTE, or `null` |
| `started` | an admin started one | it, `open`, `correct_option` null |
| `closed` | its window ended (the timer), or an admin ended it | it, `"status": "closed"`, `correct_option` set: the reveal, sent once |
| `resync` | reloaded after a failed write | the open QTE, or `null` |

The admin routes (write, start, end, list with answers and stats) are in [`internal/admin/README.md`](../admin/README.md).

## Rules and cases
| Case | Result |
|---|---|
| Kind not `quiz`, or no quiz | `ErrUnknownKind` |
| Powerup not one of teamstate's kinds | `ErrUnknownPowerup` |
| Question empty or spaces | `ErrInvalidQuestion` |
| Fewer than 2 options; an empty or repeated one | `ErrInvalidOptions` |
| Correct option < 0 or ≥ the number of options | `ErrInvalidCorrectOption` |
| Window < 1 s or > **5 min** (`qte.max_answer_window`); exactly 5 min passes | `ErrInvalidWindow` |
| Create started, or Start, while one is open | `ErrQTEOpen`, nothing published |
| Start an unknown ID / one that ran | `ErrUnknownQTE` / `ErrQTEAlreadyRun` (create a copy to ask again) |
| End an unknown ID / a draft / a closed one | `ErrUnknownQTE` / `ErrQTENotStarted` / `ErrQTEClosed` |
| 1 µs before `ends_at` | still open |
| Exactly at `ends_at` | closed: answers refused, the timer broadcasts `closed` |
| After `closed` | `GET /api/qte` and the snapshot are `null` |
| A new QTE right after one closed (or was ended) | allowed: run windows are half-open, `[started_at, ends_at)` |

## Concurrency and locking
- **Writes and the timer queue on `mu`**; reads (`ByID`, `List`, `Public`, `Snapshot`) are lock-free loads of an immutable slice.
- **Two admins start QTEs at once:** `mu` decides; the second gets `ErrQTEOpen` from memory. The exclusion constraint is the backstop.
- **End racing the timer:** whichever takes `mu` first broadcasts `closed`; `armedFor` keeps the other from sending a second.
- **qte never calls another use case**, only the hub; teamstate reads qte lock-free while holding a team's lock. So there's no lock order to get wrong.
- **An answer racing an admin's End:** `End` writes Postgres before memory, so an answer can see the QTE open in memory after `End` committed. teamstate's insert checks the stored `ends_at` and refuses it (`409 qte_closed`): the stored `ends_at` is final. See teamstate's README.

## Failure and recovery
| Case | Result |
|---|---|
| A write fails | `500` for the admin; `reconcile` reloads (Postgres wins) and broadcasts `resync` |
| Postgres unreachable | Reads keep working; writes fail |
| Restart while a QTE is open | It stays open until its stored `ends_at`; the timer is re-armed |
| Restart after it closed | It loads closed; streams reconnect to `null` |
| `qte.answer_window` changed between restarts | Only QTEs started afterwards use it |
| A stored QTE of an unknown kind or powerup | `Load` refuses to start |

## Limits and how to change them
| Limit | Where | To change |
|---|---|---|
| Default window **30 s** | `config.toml` `qte.answer_window` (> 0) | Edit, restart |
| Longest window **5 min** | `config.toml` `qte.max_answer_window` (≥ `answer_window`) | Edit, restart |
| At least 2 options | `MinOptions` | A correctness bound: fewer leaves nothing to choose |
| One QTE at a time | the exclusion constraint and `Start`'s check | A design rule |

## Not decided yet
- **The frontend shows no QTEs yet:** no quiz screen, no `qte` event handling (AGENTS.md §17, item 7).
- **Other kinds** (a QR race, say) add their columns, CHECK, JSON key and teamstate's answer check.

## Tests
`tests/qte_test.go` (real Postgres, schema `qte_test`; a fake clock like teamstate's):

| Test | Covers |
|---|---|
| `TestDraftThenStart` | a draft isn't public and sends nothing; starting it sets `ends_at` = now + `qte.answer_window`, broadcasts `started` with `"correct_option":null` in the bytes; `Public` and the snapshot are it, without the answer; `ByID` keeps the answer |
| `TestCloseTimer` | nothing 1 µs before `ends_at`; at it, `closed` with the answer; then `Public` and the snapshot are null, no timer left, nothing more sent |
| `TestOneAtATime` | a second start, or a create started, while one is open: `ErrQTEOpen` from memory, nothing published; the first can't run again; the second starts after the close; an unknown ID |
| `TestEndEarly` | End: `closed` with the answer, `ends_at` now, the timer gone, nothing at the original end; End again, a draft, an unknown ID; a new QTE starts at once |
| `TestValidation` | every draft refusal and the boundaries (2 options, a window of 1 s and of exactly `qte.max_answer_window`); a QTE's own window sets `ends_at` |
| `TestRestart` | a restart reproduces the list (JSON equal) and re-arms the open one's timer, which closes on time; one that closed while down loads closed |
| `TestReadsWithDatabaseClosed` | `List`, `ByID`, `Public`, `Snapshot` with the database closed; a write fails |
| `TestReconcile` | a draft started by hand in Postgres: `Start` fails, memory reloads, `resync` with it open |
| `TestConcurrentStarts` | 8 drafts started at once: exactly one opens |
| `TestOverlapBackstop` | a QTE open in Postgres that memory doesn't know: `Start` and `Create` with `start` pass the memory check, the exclusion constraint refuses them (`ErrQTEOpen`, nothing written), and memory reloads it (`resync`) |
| `TestEndPersistFails` | a QTE closed by hand in Postgres: `End` fails (not `ErrQTEClosed`), memory reloads it closed (`resync` with `null`), no second reveal |
| `TestLoadRefusesUnknown` | `Load` refuses a stored powerup the rules don't list, and a kind the code doesn't have (the CHECK dropped to plant one) |
| `TestTimerFiresEarly` | a timer firing before `ends_at` (the fake clock's `fireNow`) re-arms, publishes nothing; the QTE closes once, on time |
| `TestStaleTimer` | a stop that came too late (the fake clock's `leaky`): the ended QTE's timer fires while another is open, and does nothing |
| `TestTimerAfterClose` | a timer firing after `Close` (`leaky`) does nothing |
| `TestWriteAfterClose` | a write after `Close` (a request in flight at shutdown) applies and publishes, but arms no timer |
| `TestLoadReadErrors` | `Load` fails (`load qte: …`) with the table missing (refused at prepare) and with a row failing as it streams (a view dividing by zero) |
| `TestSeededDrafts` | the dev seed: drafts 1–4, one per powerup; an admin's next QTE is id 5; a rerun refreshes a seeded draft, not one that has run |
