# user

Accounts: one per Google account (keyed by Google's subject ID), each with a profile (email, name, avatar) and a role.

This use case reads no `config.toml` keys of its own. The few numbers below that come from `config.toml` belong to other use cases and are shown with their key, e.g. **12 h** (`auth.token_ttl`).

## Contents
1. [Concepts](#concepts)
2. [The flow](#the-flow)
3. [How each file works](#how-each-file-works)
4. [Where state lives, and why Postgres only](#where-state-lives-and-why-postgres-only)
5. [Data model: Postgres](#data-model-postgres)
6. [Walkthroughs](#walkthroughs)
7. [API](#api)
8. [Rules and cases](#rules-and-cases)
9. [Concurrency](#concurrency)
10. [Failure and recovery](#failure-and-recovery)
11. [Limits and how to change them](#limits-and-how-to-change-them)
12. [Not decided yet](#not-decided-yet)
13. [Tests](#tests)

## Concepts
| Term | Meaning |
|---|---|
| **User** | One row in `userprofile`. Identified everywhere by its `id`, a UUID made by Postgres, e.g. `3f2a9c4e-1b7d-4e8a-9c51-0d6b2e7f8a14`. |
| **Google subject** (`google_sub`) | Google's stable, never-reused ID for an account, e.g. `109876543210987654321`. The **identity key**: sign-in matches on it, never on email. Never sent to clients. |
| **Profile** | `email`, `name`, `avatar_url`. Copied from Google's verified ID token at every sign-in, overwriting what was there, except that a sign-in without a picture keeps the stored avatar. |
| **Role** | What the user may do: `PLAYER` (every new user), `HELPER` (on-ground volunteer) or `ADMIN` (runs the event). Set only by hand in Postgres; sign-in never changes it. |
| **Fresh role** | The role as stored in Postgres now. `GET /api/me` returns it. |
| **Token role** | The role copied into an access token when it was issued (see `internal/auth/README.md`). Up to **12 h** (`auth.token_ttl`) old. |

## The flow
1. **First sign-in.** Auth's Google callback calls `SignInWithGoogle` with the verified profile. No row has this `google_sub`, so one is inserted with role `PLAYER`, `created_at` and `last_login_at` = now.
2. **Redemption.** Auth's `POST /auth/token` calls `Get(id)` (inside the code's redemption transaction) and puts the user, with its current role, in the response and in the access token.
3. **Later sign-ins.** `SignInWithGoogle` finds the `google_sub`, overwrites `email` and `name` with what Google says now, overwrites `avatar_url` only if Google sent a picture, and sets `last_login_at`. `id`, `role` and `created_at` stay. Auth then hands the returned user to its sign-in hook, so teams' in-memory copy of the name and avatar updates at once (`teams.Store.RefreshProfile`).
4. **Role change.** An organiser runs SQL by hand, e.g. `UPDATE userprofile SET role = 'HELPER' WHERE email = 'asha.rao@snu.edu.in'`. Users aren't held in memory, so this is safe while the backend runs.
5. **Reading yourself.** `GET /api/me` reads the row fresh, so it shows the new role at once, even though the user's token still carries the old one until their next sign-in.

**Why match on `google_sub`, not email.** Google guarantees the subject never changes or gets reused; an email can be renamed, and a deleted address can in principle be given to someone else. Matching on email would let a recycled address take over an account.

**Why the role is never touched by sign-in.** A promotion made by hand must survive the user signing in again. The upsert's `DO UPDATE` lists only the profile columns and `last_login_at`.

**Why a missing picture keeps the avatar.** Google omits `picture` at times (privacy settings, some account types). Wiping a stored avatar on every such sign-in would make it flicker; a new picture still replaces it.

**Why an email clash is its own error.** `email` is unique. A second Google account reporting an address another user already has (an address moved between accounts) can't sign in; `ErrEmailInUse` lets auth show `reason=email_in_use` instead of a generic `internal_error`.

**Why `/api/me` reads Postgres.** It's the one place the frontend can learn a role change without waiting for a new token. It's called rarely, so the read costs nothing that matters.

## How each file works

### `api.go`: routes
- **`API`** holds `*Users` and the logger. **`NewAPI(users, log)`** builds it; `server.Build` calls it with the same `*Users` it gives auth.
- **`Routes(mux, authed)`** registers `GET /api/me`, wrapped in auth's middleware (`authAPI.Authed`).
- **`me`** handles `GET /api/me`:
  1. `identity.UserID(r)`: the caller's ID from their verified token (set by auth's `RequireAuth`).
  2. `users.Get(ctx, id)`.
  3. Any error goes to `fail`. `ErrUserNotFound`: `401 user_not_found` (`user no longer exists`). Any other error: logged as `load user`, `500 internal error`.
  4. `200` with the `User`.

  It ignores the token's role, email and name: everything comes from the row.
- **`errorCodes`** maps rule errors to a status and code, as in every use case. One row: `ErrUserNotFound` → `401 user_not_found`, message `user no longer exists`.
  - **Why `401`, not `404`.** The caller's token is valid but names a user that no longer exists. That's an authentication failure: the frontend should drop the token and sign in again. `404` would suggest the route or resource is missing.
  - **Compare `POST /auth/token`**, which answers `404 user_not_found` when a **login code's** user was deleted: no token is involved there, the code simply points at nothing (see `internal/auth/README.md`).
- **`fail(w, err)`** writes the matching row (message = the error's text), or logs `load user` and writes `500 internal error`.

### `read.go`: types and reads
- **`Role`** is a `string` type. Constants `RolePlayer = "PLAYER"`, `RoleHelper = "HELPER"`, `RoleAdmin = "ADMIN"`, matching the `user_role` enum.
- **`User`**: the JSON and Go shape of a user.

  | Field | Go type | JSON | Meaning |
  |---|---|---|---|
  | `ID` | `uuid.UUID` | `id` | Primary key |
  | `Email` | `string` | `email` | From Google, latest sign-in |
  | `Name` | `string` | `name` | From Google, latest sign-in; may be `""` |
  | `AvatarURL` | `*string` | `avatar_url` | From Google's `picture` at the latest sign-in that sent one; `null` if none ever did |
  | `Role` | `Role` | `role` | `PLAYER`, `HELPER` or `ADMIN` |
  | `CreatedAt` | `time.Time` | `created_at` | First sign-in |

  `google_sub` and `last_login_at` are deliberately not in `User`: they're never sent to clients.
- **`Users`** is the use case: just a `db`. **`New(pg)`** builds it from the shared pool. `server.Build` calls it first, before auth, because auth depends on it.
- **`ErrUserNotFound`** (`user no longer exists`): no user has that ID.
- **`Get(ctx, id)`** → `db.selectByID`. Returns the user, or `ErrUserNotFound` wrapping `postgres.ErrNotFound` (so callers may test either). Called by `me` (→ `401 user_not_found`) and by auth's `token` (which maps not found to its own `404 user_not_found`).

### `write.go`: writes
- **`GoogleProfile`** `{Subject, Email, Name, AvatarURL string}`: what Google's verified ID token says. Auth fills it from `sub`, `email`, `name`, `picture`.
- **`ErrEmailInUse`** (`email already belongs to another account`): a different Google account's user already has this email. Auth maps it to `reason=email_in_use`.
- **`SignInWithGoogle(ctx, p)`** → `db.upsertGoogle(p)`. Returns the stored user (with the avatar actually stored, so a kept avatar is returned too), or `ErrEmailInUse`. Called by auth's `callback`; tests also call it directly (`newPlayer` in `internal/server`). It doesn't validate anything itself: auth has already checked that the email is verified, non-empty and in an allowed domain.

### `db.go`: SQL
One `db` struct holding the pool. No transactions: each method is one statement on `userprofile`.

- **`userColumns`** = `id, email, name, avatar_url, role, created_at`, the order `scanUser` reads.
- **`scanUser(row)`** scans one row into a `User`, mapping `pgx.ErrNoRows` to `postgres.ErrNotFound`.
- **`selectByID(id)`**: `SELECT <userColumns> FROM userprofile WHERE id = $1`. A primary-key lookup.
- **`upsertGoogle(p)`**: turns an empty `AvatarURL` into SQL `NULL`, then:
  ```sql
  INSERT INTO userprofile (google_sub, email, name, avatar_url)
  VALUES ($1, $2, $3, $4)
  ON CONFLICT (google_sub) DO UPDATE SET
      email         = EXCLUDED.email,
      name          = EXCLUDED.name,
      avatar_url    = COALESCE(EXCLUDED.avatar_url, userprofile.avatar_url),
      last_login_at = now()
  RETURNING id, email, name, avatar_url, role, created_at
  ```
  On insert, `id`, `role`, `created_at` and `last_login_at` take their defaults. On conflict, only the four listed columns change; `avatar_url` only when the new value isn't `NULL`. A conflict on `email` (not `google_sub`) is a unique violation (`23505`) on `userprofile_email_key` (**`emailConstraint`**, Postgres's default name for 0001's `UNIQUE`): it's returned as `ErrEmailInUse`. Any other error is returned as is.

### `doc.go`
The package comment.

## Where state lives, and why Postgres only
**Postgres only, nothing in memory.** There's no `store.go`, no write-through and no reconcile: `read.go` and `write.go` call `db.go` directly.

**Why:** users are read and written rarely. A user signs in about once per **12 h** (`auth.token_ttl`), and `/api/me` is called on demand, not per scan. Requests that only need "who is this" get it from the token, with no database read. Caching users would add locking and restart logic for no gain.

**A consequence:** users can be edited with SQL while the backend runs (roles by hand, `cmd/loadtest` inserting players in bulk), unlike teams and team state. Teams caches members' names and avatars in memory; auth's sign-in hook refreshes that cache after every sign-in (see `internal/teams/README.md`).

## Data model: Postgres
Migration `0001_users.sql`.

### Enum `user_role`
`'PLAYER'`, `'HELPER'`, `'ADMIN'`. Backs the rule that a role is one of exactly three values; any other string is refused by Postgres.

### `userprofile`
| Column | Type | Constraint | Rule it backs |
|---|---|---|---|
| `id` | `UUID` | `PRIMARY KEY DEFAULT gen_random_uuid()` | Every user has a stable, unguessable ID, made by Postgres. Used as the token's `sub` and in every other table. |
| `google_sub` | `TEXT` | `NOT NULL UNIQUE` | One account per Google account. Also the `ON CONFLICT` target, so the unique index is what makes sign-in an upsert. |
| `email` | `TEXT` | `NOT NULL UNIQUE` (`userprofile_email_key`) | No two accounts share an email; a clash is `ErrEmailInUse`. Case-sensitive (plain `TEXT`). |
| `name` | `TEXT` | `NOT NULL DEFAULT ''` | Always a string. The upsert always passes a value, so the default only matters for hand-inserted rows. |
| `avatar_url` | `TEXT` | nullable | No avatar is `NULL`, never `""`. |
| `role` | `user_role` | `NOT NULL DEFAULT 'PLAYER'` | Every new user is a player. The upsert never sets it. |
| `created_at` | `TIMESTAMPTZ` | `NOT NULL DEFAULT now()` | First sign-in time. Never updated. |
| `last_login_at` | `TIMESTAMPTZ` | `NOT NULL DEFAULT now()` | Set on insert and on every later sign-in. Written, but not read by any code. |

### Tables that reference `userprofile (id)`
| Table | Column | Migration | On user delete |
|---|---|---|---|
| `auth_code` | `user_id` | `0001` | `CASCADE`: codes deleted |
| `teammember` | `user_id` (also `UNIQUE`) | `0002` | `CASCADE`: membership deleted |
| `team` | `leader_id` | `0002` | **Refused** (no action) while they lead a team |
| `team_scan` | `user_id` | `0003` | **Refused** once they've had any scan accepted |
| `team_selection` | `selected_by` | `0004` | **Refused** while a selection they made exists |

So in practice a user who has played can't be deleted without first deleting their team's rows.

## Walkthroughs
Google account `sub 109876543210987654321`, `asha.rao@snu.edu.in`. Times are 2026-10-17, UTC.

### 1. First sign-in, a second sign-in, a promotion
| Time | Call | Check | `userprofile` after | Result |
|---|---|---|---|---|
| 10:00:03 | `SignInWithGoogle{sub 1098…321, asha.rao@snu.edu.in, "Asha Rao", https://lh3.googleusercontent.com/a/ACg8ocK1}` | no row with this `google_sub`: insert | `id 3f2a9c4e-…8a14`, role `PLAYER`, `created_at` = `last_login_at` = 10:00:03 | user, role `PLAYER` |
| 10:00:05 | `GET /api/me`, token for `3f2a…8a14` | row found | unchanged | `200`, role `PLAYER` |
| 11:30:00 | (by hand) `UPDATE userprofile SET role = 'HELPER' WHERE id = '3f2a…8a14'` | | role `HELPER` | |
| 11:30:10 | `GET /api/me`, same token (its `role` claim is still `PLAYER`) | row found | unchanged | `200`, role **`HELPER`** |
| 18:00:00 | `SignInWithGoogle{sub 1098…321, asha.rao@snu.edu.in, "Asha R.", ""}` (renamed; Google sent no picture) | `google_sub` exists: update | name `Asha R.`, `avatar_url` **kept** `https://lh3.googleusercontent.com/a/ACg8ocK1`, `last_login_at` 18:00:00; `id`, role `HELPER`, `created_at` unchanged | user, role `HELPER`; the new token carries `HELPER`; her team shows `Asha R.` at once |

### 2. Two Google accounts, one email
| Time | Call | Result |
|---|---|---|
| 10:00 | `SignInWithGoogle{sub A, x@snu.edu.in}` | Row 1 |
| 12:00 | `SignInWithGoogle{sub B, x@snu.edu.in}` | `INSERT` conflicts on `email`, not `google_sub`: `ErrEmailInUse`. Auth redirects with `reason=email_in_use`. Row 1 untouched. |
| 12:05 | `SignInWithGoogle{sub A, x.new@snu.edu.in}` (A renamed its address) | Row 1's email becomes `x.new@snu.edu.in` |
| 12:06 | `SignInWithGoogle{sub B, x@snu.edu.in}` | Now succeeds: Row 2 |

## API
Every response uses the `httpx` envelope `{"success", "code", "message", "payload"}`, empty fields omitted, with `Cache-Control: no-store`. Every error except `500` has a stable `code`.

| Method | Path | Auth | Body | Response payload |
|---|---|---|---|---|
| GET | `/api/me` | `Authorization: Bearer <jwt>` | none | `User` |

### `GET /api/me`
Request:
```
GET /api/me HTTP/1.1
Host: localhost:3000
Authorization: Bearer eyJhbGciOiJFZERTQSIsInR5cCI6IkpXVCJ9.<payload>.<signature>
```
`200`:
```json
{
  "success": true,
  "payload": {
    "id": "3f2a9c4e-1b7d-4e8a-9c51-0d6b2e7f8a14",
    "email": "asha.rao@snu.edu.in",
    "name": "Asha Rao",
    "avatar_url": "https://lh3.googleusercontent.com/a/ACg8ocK1",
    "role": "HELPER",
    "created_at": "2026-10-17T10:00:03.204117Z"
  }
}
```
- `avatar_url` is `null` when there's none.
- `role` is read from Postgres now, not from the token.
- `created_at` has Postgres's microsecond precision, in the backend's local time zone.
- The same object is `payload.user` in auth's `POST /auth/token`.

`401`, no or bad token (from auth's middleware; see `internal/auth/README.md`):
```json
{"success": false, "code": "missing_token", "message": "missing bearer token"}
```
```json
{"success": false, "code": "invalid_token", "message": "invalid or expired token"}
```
`401`, valid token but the row is gone (an auth failure: sign in again; `POST /auth/token` uses `404` for the same code when a login code's user is gone):
```json
{"success": false, "code": "user_not_found", "message": "user no longer exists"}
```
`500`:
```json
{"success": false, "message": "internal error"}
```
Any other method (`POST /api/me`): the server's `405 method_not_allowed` envelope with `Allow: GET, HEAD` (see `internal/server/README.md`).

**Live events:** none.

## Rules and cases

### `GET /api/me`
Checked in this order:

| Case | Result |
|---|---|
| No `Authorization`, or not `Bearer <token>` | `401 missing_token` (auth) |
| Token invalid, expired, or signed by another key | `401 invalid_token` (auth) |
| Token valid, no row with its `sub` (user deleted) | `401 user_not_found` |
| Database error | `500 internal error`, logged |
| Role changed since the token was issued | `200` with the **new** role |
| No avatar | `200`, `"avatar_url": null` |
| Otherwise | `200` with the user |

### `SignInWithGoogle`
Called only after auth has verified the email (see `internal/auth/README.md`).

| Case | Result |
|---|---|
| New `google_sub`, unused email | Row inserted: role `PLAYER`, `created_at` = `last_login_at` = now |
| Known `google_sub` | `email`, `name` overwritten; `avatar_url` overwritten if a picture was sent; `last_login_at` = now; `id`, `role`, `created_at` unchanged |
| Known `google_sub`, Google reports a new email | Email updated (matching is by subject) |
| Known `google_sub`, its new email belongs to another row | `ErrEmailInUse` → auth's `reason=email_in_use`; nothing changed |
| New `google_sub`, email already belongs to another row | `ErrEmailInUse` → `reason=email_in_use`; nothing inserted |
| Same email in another case (`Asha.Rao@…` vs `asha.rao@…`) | Treated as different (case-sensitive `TEXT`) |
| `AvatarURL` empty, first sign-in | Stored as `NULL` |
| `AvatarURL` empty, later sign-in | The stored avatar is **kept** (`COALESCE`) |
| `AvatarURL` set | Stored, replacing any earlier avatar |
| `Name` empty | Stored as `""` |
| Another unique violation or database error | Error → `reason=internal_error` |

### `Get`
| Case | Result |
|---|---|
| Row exists | The user |
| No row | `ErrUserNotFound`, wrapping `postgres.ErrNotFound` |
| Database error | The error |

### General rules
- One account per Google subject; one account per email.
- Roles change only by hand in Postgres. No route changes them.
- Code that gates on role must read it with `Get`; the token's role (`identity.Role`) can be up to **12 h** (`auth.token_ttl`) stale.
- `google_sub` and `last_login_at` are never sent to clients. `/api/me` sends the caller only their own email.

## Concurrency
No locks and no memory: Postgres settles every race.

| Race | Result |
|---|---|
| Two first sign-ins of one Google account at once | `INSERT … ON CONFLICT (google_sub)`: one insert, the other updates the same row. Both return the same `id`. |
| Two different Google accounts with one email at once | One inserts; the other hits the `email` unique index: `ErrEmailInUse`. |
| A sign-in and a hand-made role change at once | Both land: the upsert never writes `role`, so it can't overwrite the change. |
| `/api/me` during a role change | Sees the role before or after the `UPDATE` commits, never anything else. |

## Failure and recovery
| Case | Result |
|---|---|
| Postgres unreachable | `/api/me` → `500`; sign-ins → auth's `reason=internal_error`. Nothing to reconcile. |
| Backend restarts | Nothing to rebuild; every read goes to Postgres. |
| Rows edited by hand while running | Safe for this use case: the next read sees them. Teams' in-memory member names don't see a hand edit until that user signs in again or the backend restarts. |
| A user is deleted by hand | Their tokens still pass auth's middleware; `/api/me` returns `401 user_not_found`; their login codes are deleted by cascade. Refused if they lead a team, have scans, or made a selection. |
| `auth.token_ttl` changed | Changes how long a token's role can lag; nothing in this use case. |

## Limits and how to change them
| Limit | Where | To change |
|---|---|---|
| Roles: exactly 3 | `user_role` enum (`0001`) and the `Role` constants | A new migration with `ALTER TYPE user_role ADD VALUE`, plus a constant, plus the frontend's `UserRoleSchema` |
| Email, name, avatar length | Unbounded `TEXT` | Google's values are short; no limit is enforced |
| How stale a token's role can be: **12 h** | `config.toml` `auth.token_ttl` | Edit and restart |

## Not decided yet
- **An email clash blocks sign-in.** A Google account whose email belongs to another row gets `reason=email_in_use` and can't sign in until the other account's email changes. Whether to drop the `email` unique constraint, or let an organiser merge accounts, is open.
- **A removed avatar can't be cleared.** Since an empty `picture` keeps the stored avatar, a user who deletes their Google photo keeps the old URL (which may stop loading).
- **Emails are case-sensitive.** Google normally sends lowercase, but nothing enforces it.
- **No role management.** Roles are set by SQL. No admin route exists.
- **No account deletion.** Deleting a user who has played is blocked by foreign keys without `ON DELETE` actions on `team`, `team_scan` and `team_selection`.
- **`last_login_at` is written but never read.**

## Tests
`tests/store_test.go` (real Postgres, schema `user_test`):

| Test | Covers |
|---|---|
| `TestSignInCreatesThenRefreshes` | First sign-in creates a `PLAYER` with no avatar; after a hand-made `ADMIN`, signing in with a new email and name keeps the `id` and role and updates both |
| `TestSignInWithoutPictureKeepsAvatar` | An empty picture keeps `https://x/1.png`; a new picture `https://x/2.png` replaces it |
| `TestEmailInUse` | A new subject with a taken email, and an existing user changing to a taken email, both get `ErrEmailInUse` |
| `TestGetUnknownUser` | `Get` of a random UUID → `ErrUserNotFound`, which also matches `postgres.ErrNotFound` |
| `TestConcurrentFirstSignIns` | 8 concurrent first sign-ins of one subject: one row, every call gets the same `id` |

End to end in `internal/server` (real Postgres):

| Test | Covers |
|---|---|
| `internal/server/tests/auth_test.go` `TestFullLoginFlow` | first sign-in creates a `PLAYER`; `/api/me` returns name and role; a role change made with SQL shows on `/api/me` without a new token; signing in again keeps the same `id` and role and refreshes `name` |
| `internal/server/tests/auth_test.go` `TestTokenAndAuthErrors` | `SignInWithGoogle` called directly; `/api/me` with no token → `401 missing_token`, with a garbage token → `401 invalid_token`; `POST /api/me` → `405` |
| `internal/server/tests/auth_test.go` `TestEmailInUse` | A sign-in whose email another account has → `reason=email_in_use` |
| `internal/server/tests/auth_test.go` `TestSignInRefreshesTeamProfile` | A second sign-in with a new name and no picture: the team shows the new name and keeps the avatar |
| `internal/server/tests/teams_test.go` `newPlayer` helper | every team and team-state test creates its users through `SignInWithGoogle` |

Not covered by any test yet: `401 user_not_found` over HTTP, and a database error on `/api/me`.
