# QR: rotating waypoint tokens

The `qr` use case owns the short-lived, rotating tokens a waypoint's QR code
shows. Config values appear with their key: **5 s** (`qr.rotate`) and **5 s**
(`qr.tolerance`).

## Concepts

| Term | Meaning |
|---|---|
| Waypoint | A physical QR location, 4-digit ID (`atlas` use case). |
| Token | The signed string a waypoint's QR encodes: `"<waypoint>:<unix seconds>.<base64url HMAC>"`. |
| Signing key | A per-waypoint HMAC-SHA256 key, derived from `QR_SECRET` as `HMAC(QR_SECRET, "qr:<waypoint>")`. The display signs with it. |
| Display | The staff's `/qr` page: a screen at a waypoint showing its rotating QR. |
| Window | How long a token is valid after issue: `qr.rotate` + `qr.tolerance` = **10 s**. |
| Staff | A HELPER or ADMIN role; players can't mint a key (they'd skip the physical scan). |

## The flow

1. A staff member opens the `/qr` page, enters a waypoint's 4-digit ID, and
   leaves the screen at that waypoint.
2. The page fetches `GET /api/qr/{waypoint_id}` **once**: the waypoint-scoped
   signing key and the timing rules (`rotate_ms`, `window_ms`, `server_time`).
3. The page signs `"<waypoint>:<unix seconds>"` with that key every **5 s**
   (`qr.rotate`) and renders it as a QR. No further requests.
4. A player scans the QR; their phone reads the token and submits it.
5. The backend derives the same per-waypoint key, verifies the signature, and
   checks the token is within the window. `teamstate.Scan` does this through
   the `QR` interface it declares (`Verify`), then resolves the waypoint it
   names.

**Why the token rotates.** A screenshot stops working: a friend who receives a
photo must scan and submit within **10 s** of issue. **Why the display signs
locally.** Signing needs no secret from the server each rotation, so the
display makes one request and then works offline for as long as its clock is
right. **Why the key is per-waypoint.** A display for `1001` can't sign tokens
for `1002`: the key is `HMAC(QR_SECRET, "qr:1001")`, scoped to that waypoint.
**Why minting is staff-only.** If players could call `GET /api/qr/{id}`, they
could sign their own tokens for any waypoint without being there, defeating the
purpose.

## How each file works

- `doc.go`: the package comment.
- `read.go`: the `Verifier` (secret, rules, clock), `New`, `keyFor` (the
  per-waypoint key), `sign` (HMAC-SHA256), `Verify`, and the errors `ErrInvalid`
  and `ErrExpired`.
- `write.go`: `SigningKey` and its JSON, `ErrUnknownWaypoint`.
- `api.go`: the HTTP routes (`Routes` for authed, `StaffRoutes` for staff), the
  handlers `issue` and `verify`, and the `errorCodes` table.
- `db.go`: none. A comment only: the use case has no SQL.
- `README.md`: this file.
- `tests/`: the signing-key round trip and the API guard/verify tests over HTTP.

## Data model: memory

None. There is no `store.go`: a signing key and a token are derived on demand
from the secret and the clock, so nothing is held between requests and nothing
needs a lock. One `Verifier` is shared by every goroutine and is safe to read
concurrently.

## Data model: Postgres

None. No tables are read or written.

## Memory ↔ Postgres

Not applicable: nothing is persisted, so a restart has nothing to reload.

## Walkthroughs

`qr.rotate = 5s`, `qr.tolerance = 5s`, so `window = 10s`.

| Step | Action | Result |
|---|---|---|
| 1 | Staff opens `/qr`, enters `1001`. Page calls `GET /api/qr/1001`. | `SigningKey` checks `1001` is in the atlas and returns `{"waypoint_id":"1001","key":"<64 hex chars>","rotate_ms":5000,"window_ms":10000,"server_time":1760000000}`. |
| 2 | Page signs `1001:1760000000` with the key and renders the QR. | The QR shows `1001:1760000000.<base64url sig>`. |
| 3 | Player scans and submits the token at `t = 1760000003`. | `Verify` derives the `1001` key, recomputes the HMAC (match), checks `t < issued+10s` (yes): returns `"1001"`. |
| 4 | A friend scans a photo of the same QR at `t = 1760000011`. | Signature OK, but `t` is past the window: `ErrExpired` → `409 qr_expired`. |

## The write-through

Not applicable. `SigningKey` signs but changes no state and writes nothing, so
there are no lock/validate/build/persist/apply/publish steps and no reconcile.

## API

| Method | Path | Auth | Description |
|---|---|---|---|
| GET | `/api/qr/{waypoint_id}` | Bearer + HELPER/ADMIN | The waypoint's signing key and timing rules. |
| POST | `/api/qr/verify` | Bearer | `{"qr"}` → `{"waypoint_id"}`. Changes nothing. |

```json
// GET /api/qr/1001 → 200
{"success": true, "payload": {
  "waypoint_id": "1001",
  "key": "9f3a…64 hex chars",
  "rotate_ms": 5000,
  "window_ms": 10000,
  "server_time": 1760000000
}}

// POST /api/qr/verify {"qr":"1001:1760000000.nR5…"} → 200
{"success": true, "payload": {"waypoint_id": "1001"}}

// errors
{"success": false, "code": "invalid_qr", "message": "invalid qr token"}          // 400
{"success": false, "code": "qr_expired", "message": "qr token expired"}          // 409
{"success": false, "code": "unknown_waypoint", "message": "unknown waypoint"}    // 404 (mint)
{"success": false, "code": "invalid_waypoint_id", "message": "waypoint id must be 4 digits"} // 400 (mint)
{"success": false, "code": "not_staff", "message": "only staff can do this"}     // 403 (mint)
{"success": false, "code": "invalid_body", "message": "body must be {\"qr\": string}"} // 400 (verify)
```

## Rules and cases

**`SigningKey(waypointID)`**

| Case | Result |
|---|---|
| Waypoint exists | Returns its key and the timing rules. |
| Unknown waypoint | `ErrUnknownWaypoint` → `404 unknown_waypoint`. |
| ID not 4 digits | The route rejects it first: `400 invalid_waypoint_id`. |

**`Verify(token)`**, in this order:

| Case | Result |
|---|---|
| No `.` separator, empty waypoint, or non-numeric seconds | `ErrInvalid` → `400 invalid_qr`. |
| Signature isn't canonical base64url: not base64url at all, or its last character has non-zero filler bits (see below) | `ErrInvalid`. |
| Signature doesn't match (tampered, wrong waypoint's key, wrong secret) | `ErrInvalid`. |
| Issued in the future | `ErrExpired` → `409 qr_expired`. |
| Issued more than `window` ago | `ErrExpired`. |
| Exactly at `expires_at` | `ErrExpired` (the window is `now < expires_at`). |
| Signed and within the window | The waypoint ID. |

**Why the signature is decoded strictly** (`base64.RawURLEncoding.Strict()`). The HMAC-SHA256 signature is 32 bytes, so 43 base64url characters: 42 carry 6 bits each, and the last carries the remaining 4 bits plus 2 filler bits that the canonical encoding leaves at zero. A lenient decoder ignores the filler, so a signature ending in `A`, `B`, `C` or `D` (`000000` to `000011`) decodes to the same bytes. That's no forgery (the bytes are the real signature) but it gives every token four spellings, and it made the old tamper test flaky: flipping the last character to `A` sometimes changed only filler bits, and the "tampered" token verified (about 1 run in 16). Strict decoding accepts only the canonical spelling. The `/qr` page encodes with `btoa` and strips the `=` padding, which always yields the canonical spelling.

A resent verify is fine to repeat: it changes nothing.

## Concurrency and locking

None: `Verify` and `SigningKey` touch only their arguments and the shared
secret, which is read-only. The `Verifier` is used from many goroutines without
locks.

## Failure and recovery

The secret is read once at startup from `QR_SECRET`; if it changes, tokens
signed before the restart stop verifying (the backend refuses to start without
a valid secret). Nothing else can fail: there is no database and no network.

## Limits and how to change them

| Limit | Where | How to raise |
|---|---|---|
| Validity window | `qr.rotate` + `qr.tolerance` | Raise either in `config.toml` (restart). |
| Token format | `waypointIDPattern`, 4 digits | Fixed by the atlas waypoint format, not tunable. |
| Secret length | `ParseQRSecret`, ≥ 32 bytes | `QR_SECRET` in `.env`. |

## Not decided yet

- ⚠ The frontend `/play` page still has no camera scanner: it submits the raw
  waypoint ID, which `POST /api/teams/me/state/scans` no longer accepts
  (`400 invalid_body`). The backend verifies tokens; the play page and
  `/test-scan` need to be wired to submit `{"qr"}` before the event.
- ⚠ The signing key sits on the display's page. A player who can open `/qr`
  (staff-only in production) could sign their own tokens. The staff gate is
  what closes this; the per-waypoint key limits one display to its waypoint.
- ⚠ A token is replayable by anyone who gets it within its window. Accepted:
  the window is only **10 s**, and stopping the "screenshot shared later"
  attack is the goal.

## Tests

| Test | Covers |
|---|---|
| `TestSigningKeyAndVerify` | The returned key signs tokens `Verify` accepts; expiry boundary, tamper, wrong waypoint key, wrong secret, malformed. |
| `TestVerifyRejectsNonCanonicalSignature` | The 3 spellings of a valid signature with filler bits set in its last character: each decodes leniently to the real bytes, and each is `ErrInvalid`; the canonical token still verifies. |
| `TestSigningKeyUnknownWaypoint` | `SigningKey` of an unknown waypoint → `ErrUnknownWaypoint`. |
| `TestQRGuard` (server) | Mint needs a token and a HELPER/ADMIN role (from the DB, not the token); a player gets `403 not_staff`. |
| `TestQRVerify` (server) | `POST /api/qr/verify` over HTTP: valid, `invalid_qr` (tampered by changing the signature's first character, all of whose bits are data, so it always changes the bytes), `qr_expired`, `invalid_body`. |
