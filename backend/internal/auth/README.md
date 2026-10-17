# auth

Signs users in with Google, hands the finished sign-in to the SvelteKit server through a one-time login code, issues and verifies access tokens (JWTs), and provides the middleware that protects `/api/*`.

Every number below that comes from `config.toml` is shown with its current value and its key, e.g. **12 h** (`auth.token_ttl`). Change the file, not the code. Secrets and URLs (`JWT_PRIVATE_KEY`, `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `PUBLIC_URL`, `FRONTEND_URL`, `ALLOWED_EMAIL_DOMAINS`) come from `.env`, read by `internal/platform/config`.

## Contents
1. [Concepts](#concepts)
2. [The sign-in flow](#the-sign-in-flow)
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
| **Browser** | The player's phone or laptop. Never sees a JWT: only the state cookie (backend's) and the token cookie (SvelteKit's), both `HttpOnly`. |
| **SvelteKit server** | The frontend's server side (`FRONTEND_URL`). Redeems login codes, stores the JWT in its `labyrinth_token` cookie, verifies it locally with `JWT_PUBLIC_KEY`, and calls `/api/*` with it. |
| **Backend** | This Go server (`PUBLIC_URL`). Talks to Google, owns users, signs every JWT. |
| **Signing key** | One Ed25519 keypair. The private half (`JWT_PRIVATE_KEY`, base64 PKCS#8) is only in the backend; the public half (`JWT_PUBLIC_KEY`, base64 SPKI, `go run . pubkey`) is in the SvelteKit server. It signs both access tokens and state cookies. |
| **State** | 32 random bytes, base64url (43 chars), e.g. `l_JRQysJ3emPfW5hF3X1KGFjXgMuq5vs8u9WLMz44N4`. Sent to Google and echoed back; ties the callback to the browser that started the sign-in (CSRF protection). |
| **PKCE verifier / challenge** | The verifier is 32 random bytes, base64url (`oauth2.GenerateVerifier`). Google gets only its SHA-256 (the `S256` challenge) at login, and the verifier itself at the code exchange, so a stolen Google `code` is useless without it. |
| **State cookie** | `labyrinth_oauth_state`: a JWT (audience `labyrinth-oauth-state`) holding `{state, verifier, next}`. Lives **10 m** (`auth.oauth_state_ttl`), only on path `/auth/google`. Signed, so it can't be forged or edited; **not** encrypted. |
| **`next`** | Where the user wants to land after sign-in, e.g. `/team`. Only a same-site path survives (`SafeNext`); anything else becomes `/`. |
| **Google code** | The authorization code Google puts in the callback URL. Single-use at Google; traded server to server for Google's ID token. |
| **ID token** | Google's signed JWT about the user. We decode `sub`, `email`, `email_verified`, `name`, `picture`; other claims (e.g. `hd`) are ignored. Verified against Google's keys, issuer and our client ID. |
| **Login code** | 32 random bytes, base64url (43 chars), e.g. `OFHvhOSp16AJpSTpguPJEvDmsoB48YcLon779lfjWtY`. Hands the finished sign-in from the backend to the SvelteKit server through the browser's URL. Single-use, valid **2 m** (`auth.login_code_ttl`). Only its SHA-256 is stored. |
| **Access token** | The backend's JWT (audience `labyrinth-api`), valid **12 h** (`auth.token_ttl`). Claims: `sub` (user ID), `email`, `name`, `avatar_url`, `role`, `iss`, `aud`, `iat`, `exp`. Stateless: nothing on the server records it. |
| **Identity** | `identity.Identity{UserID, Email, Name, Role}`, put on the request context by `RequireAuth` from the token's claims. Other use cases read it with `identity.UserID(r)` and never import auth. |
| **Reason** | The snake_case word in `FRONTEND_URL/auth/error?reason=…` when the callback fails, e.g. `state_mismatch`. |
| **Sign-in hook** | `onSignIn func(user.User)`, given to `NewAPI` by `server.Build`. Called after every successful profile upsert so in-memory copies of a user's name and avatar (teams) refresh at once. |

## The sign-in flow
Hosts as in local development: browser on `http://localhost:5173` (SvelteKit, `FRONTEND_URL`), backend on `http://localhost:3000` (`PUBLIC_URL`).

```
Browser                        SvelteKit server                 Backend                          Google
   │ 1 click "Login" (plain link)                                 │                                │
   │──── GET /auth/google/login?next=%2Fteam ────────────────────▶│                                │
   │◀─── 302 Location: accounts.google.com/o/oauth2/auth?…        │                                │
   │     Set-Cookie: labyrinth_oauth_state=<JWT>; Path=/auth/google; Max-Age=600                    │
   │ 2 picks an account ─────────────────────────────────────────────────────────────────────────▶│
   │◀──── 302 Location: localhost:3000/auth/google/callback?state=…&code=4/0Ad… ───────────────────│
   │ 3 GET /auth/google/callback?state=…&code=… (+ state cookie) ▶│                                │
   │                                                              │── POST /token (code+verifier) ▶│
   │                                                              │◀── {id_token, …} ──────────────│
   │                                                              │ verify id_token, upsert user,  │
   │                                                              │ insert auth_code (hash)        │
   │◀─── 302 Location: localhost:5173/auth/callback?code=OFHv…&next=%2Fteam                         │
   │     Set-Cookie: labyrinth_oauth_state=; Max-Age=0                                              │
   │ 4 GET /auth/callback?code=…&next=/team ─▶│                   │                                │
   │                                         │── POST /auth/token {"code":"OFHv…"} ─▶│              │
   │                                         │◀── 200 {access_token, expires_at, user}              │
   │◀── 303 Location: /team                  │                                                      │
   │    Set-Cookie: labyrinth_token=<JWT>; HttpOnly; Expires=22:00:04                               │
   │ 5 GET /team (+ labyrinth_token) ───────▶│ verify JWT locally (public key), no backend call     │
   │                                         │── GET /api/me  Authorization: Bearer <JWT> ─▶│       │
   │                                         │◀── 200 {user} ──────────────────────────────│        │
```

1. **Login.** The login button is a plain link to `GET /auth/google/login?next=/team`. The backend makes a fresh state and PKCE verifier, signs `{state, verifier, next}` into the state cookie, and 302s to Google with the state, the S256 challenge and `prompt=select_account`.
2. **Google.** The user picks an account. Google redirects to `PUBLIC_URL/auth/google/callback?state=…&code=…` (or `?state=…&error=access_denied` on Cancel).
3. **Callback.** The backend clears the state cookie, checks it against the `state` parameter, trades the Google code plus the verifier for Google's ID token, checks the email, upserts the user (`user.SignInWithGoogle`), tells the sign-in hook (teams refreshes the member's name and avatar), stores a new login code's hash, and 302s to `FRONTEND_URL/auth/callback?code=<login code>&next=/team`. Any failure 302s to `FRONTEND_URL/auth/error?reason=<reason>` instead.
4. **Redeem.** The SvelteKit server's `/auth/callback` route posts the code to `POST /auth/token`, server to server. In one transaction the backend deletes the code, loads the user and signs an access token; the delete commits only if both succeed. It returns the token, its expiry and the user. SvelteKit stores the token in its `labyrinth_token` cookie (`HttpOnly`, `SameSite=Lax`, `Path=/`, expiring at `expires_at`) and 303s to `next` (after its own `safeNext`).
5. **Use.** On every page load SvelteKit's `authenticate` hook verifies the cookie's JWT with the public key (EdDSA, issuer `labyrinth-backend`, audience `labyrinth-api`) and fills `locals.user`, with no backend call. When it calls `/api/*` it sends `Authorization: Bearer <JWT>`; the backend's `RequireAuth` verifies it again.
6. **Logout.** SvelteKit's `POST /auth/logout` deletes `labyrinth_token`. Nothing happens on the backend: tokens are stateless.

**Why a one-time code instead of the JWT in the redirect.** The redirect URL lands in browser history, logs and possibly `Referer` headers. A JWT there would be usable for **12 h** (`auth.token_ttl`) by anyone who saw it. The login code is useless after one redemption or **2 m** (`auth.login_code_ttl`), and the JWT only ever travels server to server and in an `HttpOnly` cookie.

**Why the state lives in a signed cookie, not in Postgres.** The backend needs `{state, verifier, next}` between two requests from the same browser. A JWT cookie keeps it there with no table, no sweep, and no write per login attempt. The signature stops a user from editing `next` or the verifier.

**Why the cookie is limited to `/auth/google` and `SameSite=Lax`.** It's only needed on the callback. `Lax` still sends it on Google's top-level GET redirect back to us, but not on cross-site subrequests.

**Why both PKCE and a client secret.** The backend is a confidential client (it has `GOOGLE_CLIENT_SECRET`). PKCE is defence in depth: a Google code intercepted in transit can't be exchanged without the verifier, which never leaves the backend except inside the signed cookie.

**Why the state is checked before Google's `error`.** Otherwise anyone could send a victim to `/auth/google/callback?error=x` and show them a fake failure. With the state checked first, only a callback for a sign-in this browser started gets as far as reading `error`.

**Why Ed25519 and not a shared secret.** The SvelteKit server must verify tokens on every page load without calling the backend. With a public key it can verify but never mint, so a leak of the frontend's environment can't forge an `ADMIN` token. Only `EdDSA` is accepted, so `alg: none` and the HS256-with-public-key trick are both refused.

**Why fixed Google endpoints, not OIDC discovery.** `NewGoogle` hard-codes Google's endpoints and fetches signing keys lazily, so the backend starts even when Google is unreachable.

## How each file works

### `api.go`: routes and middleware
- **Constants:** `stateCookie = "labyrinth_oauth_state"`, `stateCookiePath = "/auth/google"`.
- **`API`** holds `*config.Config`, `*Google`, `*Tokens`, `*LoginCodes`, `*user.Users`, the logger and `onSignIn func(user.User)`. **`NewAPI(cfg, google, tokens, codes, users, onSignIn, log)`** builds it; `server.Build` calls it after loading teams, with `onSignIn` = `teams.Store.RefreshProfile(u.ID, u.Name, u.AvatarURL)`. A `nil` hook is allowed and skipped.
- **`Routes(mux)`** registers the three public routes. Unlike other use cases it takes no `authed` wrapper: these routes are how a user gets a token in the first place.
- **`login`** handles `GET /auth/google/login?next=…`:
  1. `randomToken()` makes the state. On failure: logged (`generate oauth state`), `500 internal error`.
  2. `oauth2.GenerateVerifier()` makes the PKCE verifier.
  3. `tokens.issueState(oauthState{state, verifier, SafeNext(next)}, auth.oauth_state_ttl)` signs the cookie value. On failure: logged, `500 internal error`.
  4. Sets the cookie: `Path=/auth/google`, `Max-Age` = **600** s (`auth.oauth_state_ttl` in whole seconds), `HttpOnly`, `SameSite=Lax`, `Secure` when `PUBLIC_URL` is `https` (`Config.SecureCookies`).
  5. `302` to `google.AuthCodeURL(state, verifier)`.
- **`callback`** handles `GET /auth/google/callback`. It reads the cookie, then **always** sets a clearing cookie (`Max-Age=0`, same path and flags) before checking anything, then checks in this order and stops at the first failure (see [Rules and cases](#get-authgooglecallback)):
  1. cookie present
  2. `tokens.verifyState(cookie)`
  3. `state` query parameter equals the cookie's, compared in constant time (`subtle.ConstantTimeCompare`)
  4. no `error` query parameter
  5. a non-empty `code` query parameter
  6. `google.Exchange(ctx, code, verifier)` (errors logged)
  7. `EmailVerified` and a non-empty email
  8. `emailAllowed(email)`
  9. `users.SignInWithGoogle(…)` with `{Subject: sub, Email, Name, AvatarURL: picture}`. `user.ErrEmailInUse` → `email_in_use`; other errors logged → `internal_error`
  10. `onSignIn(user)`, if set (cannot fail)
  11. `codes.Create(ctx, user.ID)` (errors logged)

  On success it logs `login user_id=…` and calls `redirectFrontend(…, "/auth/callback", {code, next})`. Every failure goes through `redirectError`.
- **`tokenResponse`** is the payload of a successful `POST /auth/token`: `access_token`, `token_type`, `expires_at`, `user`.
- **`errorCodes`** maps the rule errors (declared in `write.go`) to a status and code, as in every use case:

  | Error | Status | Code | Message |
  |---|---|---|---|
  | `ErrInvalidCode` | `400` | `invalid_code` | `invalid or expired code` |
  | `errUserGone` | `404` | `user_not_found` | `the code's user no longer exists` |

  Only `POST /auth/token` uses it. The callback's failures are redirect reasons (`/auth/error?reason=…`), not envelope errors; `invalid_body` and the middleware's `missing_token` / `invalid_token` are written where they happen.
- **`fail(w, err)`** writes the matching row (message = the error's text), or logs `redeem login code` and writes `500 internal error`.
- **`token`** handles `POST /auth/token {"code"}`:
  1. `httpx.DecodeJSON` into `{Code string}`. A decode error or an empty `code`: `400 invalid_body`.
  2. `codes.Redeem(code, use)`, where `use(userID)`:
     1. `users.Get(userID)`. Not found (`user.ErrUserNotFound`, which wraps `postgres.ErrNotFound`) → `errUserGone`; other errors wrapped as `load user: …`.
     2. `tokens.IssueAccess(user)`. Error wrapped as `sign access token: …`.
     3. Keeps `tokenResponse{token, "Bearer", exp, user}`.
  3. Any error from `Redeem` goes to `fail`: `ErrInvalidCode` → `400 invalid_code`; `errUserGone` → `404 user_not_found`; anything else → logged (`redeem login code`), `500 internal error`. If `use` failed, the code was **not** used up.
  4. `200` with the kept `tokenResponse`.
- **`RequireAuth(next)`** is the middleware on every `/api/*` route:
  1. Splits `Authorization` at the first space. If there's no space, the scheme isn't `Bearer` (case-insensitive), or the rest is empty: `WWW-Authenticate: Bearer`, `401 missing_token`.
  2. `tokens.VerifyAccess(raw)`. On error: `WWW-Authenticate: Bearer error="invalid_token"`, `401 invalid_token`.
  3. Puts `identity.Identity{UserID, Email, Name, Role}` from the claims on the context and calls `next`.
- **`Authed(h)`** is `RequireAuth(h)` shaped as `func(http.HandlerFunc) http.Handler`. `server.Build` passes it to `user`, `teams` and `teamstate` `Routes`, and wraps `GET /api/teams/me/events` with it.
- **`emailAllowed(email)`**: `true` when `ALLOWED_EMAIL_DOMAINS` is empty. Otherwise lowercases the email, takes everything after the first `@`, and needs an **exact** match with one listed domain (`snu.edu.in` doesn't allow `cse.snu.edu.in`). `config.Load` lowercases and trims the list.
- **`redirectError(reason)`** logs `login failed reason=…` at warn level and redirects to `/auth/error?reason=…`.
- **`redirectFrontend(path, q)`** copies `FRONTEND_URL`, appends `path` to its path (trailing slashes trimmed), sets the encoded query (`url.Values.Encode`: keys sorted, `/` escaped as `%2F`) and sends `302 Found`.
- **`SafeNext(next)`** (exported; tested directly) returns `next` only if it starts with `/`, doesn't start with `//`, contains none of `\`, `\r`, `\n`, `\t`, parses with `url.Parse`, and has no scheme or host. Otherwise `/`.

### `read.go`: token types and verification
- **Constants:** `issuer = "labyrinth-backend"`, `accessAudience = "labyrinth-api"`, `stateAudience = "labyrinth-oauth-state"`. **`signingMethod`** is `jwt.SigningMethodEdDSA`, the only algorithm signed or accepted.
- **`Tokens`** holds the Ed25519 private key, its public half, and the access-token TTL. **`NewTokens(key, ttl)`** builds it; `server.Build` passes `JWT_PRIVATE_KEY` and **12 h** (`auth.token_ttl`). It's immutable, so safe for concurrent use.
- **`AccessClaims`**: `Email`, `Name`, `AvatarURL *string`, `Role user.Role`, plus `jwt.RegisteredClaims`. **`UserID()`** parses `sub` as a UUID.
- **`VerifyAccess(raw)`** parses with `jwt.ParseWithClaims` and requires: algorithm `EdDSA` only; signature valid under the public key; `iss = labyrinth-backend`; `aud` contains `labyrinth-api`; `exp` present and `now < exp` (no leeway); `nbf`, if present, passed. Then it requires `sub` to parse as a UUID (`token subject is not a user id`). `iat` isn't checked.
- **`oauthState`**: `State`, `Verifier`, `Next`, plus `jwt.RegisteredClaims`. Only `login` and `callback` use it.
- **`verifyState(raw)`** is `VerifyAccess` with audience `labyrinth-oauth-state` and no subject check. Because the audiences differ, neither kind of token is accepted as the other.
- **`keyFunc`** returns the public key for the parser.

### `write.go`: signing and login codes
- **Rule errors:** `ErrInvalidCode` (`invalid or expired code`): the code is unknown, expired or already used. `errUserGone` (`the code's user no longer exists`): returned by `token`'s `use` from inside the redemption when the code's user is missing, so it isn't confused with `ErrInvalidCode`. Both are mapped in `api.go`'s `errorCodes`.
- **`IssueAccess(u)`** takes `now` truncated to whole seconds, signs `AccessClaims` for `u` with `iat = now`, `exp = now + 12 h` (`auth.token_ttl`), and returns `(token, exp, err)`. Because `now` is whole seconds, the returned `exp` (the response's `expires_at`) equals the token's `exp` claim exactly. Called by `token`, by `internal/server` tests (`newPlayer`) and by `cmd/loadtest`.
- **`issueState(s, ttl)`** sets `iss`, `aud = [labyrinth-oauth-state]`, `iat` and `exp = now + ttl` on `s` and signs it.
- **`LoginCodes`** holds the `db` and the code TTL. **`NewLoginCodes(pg, ttl)`** builds it; `server.Build` passes **2 m** (`auth.login_code_ttl`).
  - **`Create(ctx, userID)`**: `randomToken()`, then `db.insertCode(code, userID, ttl)`. Returns the plain code; it exists in plain text only in the redirect URL. Called by `callback`.
  - **`Redeem(ctx, code, use func(uuid.UUID) error)`**: `db.consumeCode(code, use)`. Calls `use` with the code's user inside the transaction that deletes the code. Returns `nil` (code used up), `use`'s error (code kept), or `ErrInvalidCode` wrapping `postgres.ErrNotFound` (`consumeCode`'s "no row") if the code is unknown, expired or already used. Called by `token`.
  - **`DeleteExpired(ctx)`**: `db.deleteExpiredCodes()`, returns how many rows went. Called by `main.cleanupLoginCodes` every **10 m** (`auth.login_code_sweep`), first one interval after start, and by tests.
- **`tokenBytes = 32`**: 256 bits for states and login codes. A security parameter, not a tunable, so it isn't in `config.toml`.
- **`randomToken()`**: `tokenBytes` from `crypto/rand`, base64url without padding (43 chars).

### `db.go`: SQL
One `db` struct holding the pool. Each method is one statement on `auth_code`; only `consumeCode` uses a transaction:

| Method | SQL | Notes |
|---|---|---|
| `insertCode(code, userID, ttl)` | `INSERT INTO auth_code (code_hash, user_id, expires_at) VALUES ($1, $2, now() + $3::interval)` | `$1` = SHA-256 of the code (32 bytes). `$3` = the TTL. Expiry uses **Postgres's** clock, the same one `consumeCode` and the sweep compare against, so backend clock skew doesn't matter. |
| `consumeCode(code, use)` | In one `pgx.BeginFunc` transaction: `DELETE FROM auth_code WHERE code_hash = $1 AND expires_at > now() RETURNING user_id`, then `use(userID)` | No row → `postgres.ErrNotFound`. If `use` returns an error the transaction rolls back and the row is back, still redeemable. The `DELETE` row-locks the code, so a concurrent redemption waits, then finds no row once the first commits (or redeems it if the first rolled back). |
| `deleteExpiredCodes()` | `DELETE FROM auth_code WHERE expires_at <= now()` | Returns rows affected. Uses `auth_code_expires_at_idx`. |

### `google.go`: Google client (supporting file)
- **`GoogleEndpoints`** `{OAuth oauth2.Endpoint, Issuer, JWKSURL}`. Tests point them at a fake provider.
- **`DefaultGoogleEndpoints`**: auth `https://accounts.google.com/o/oauth2/auth`, token `https://oauth2.googleapis.com/token`, issuer `https://accounts.google.com`, keys `https://www.googleapis.com/oauth2/v3/certs`. `main` passes these to `server.Build`.
- **`Google`** holds the `oauth2.Config` and an `oidc.IDTokenVerifier`.
- **`GoogleIdentity`**: `sub`, `email`, `email_verified`, `name`, `picture`. Nothing else is decoded (no `hd`).
- **`NewGoogle(clientID, secret, redirectURL, ep)`**: scopes `openid email profile`; redirect URL is `PUBLIC_URL + "/auth/google/callback"` (from `server.Build`); a remote key set fetched lazily from `JWKSURL`; a verifier for `Issuer` with audience = client ID. Default algorithm: RS256.
- **`AuthCodeURL(state, verifier)`**: the consent URL with `code_challenge` (S256 of the verifier), `code_challenge_method=S256` and `prompt=select_account` (always show the account chooser, so a shared phone can switch accounts).
- **`Exchange(ctx, code, verifier)`**: posts the code and `code_verifier` to the token endpoint, requires an `id_token` in the response, verifies its signature, issuer, audience and expiry, and decodes the claims. No nonce is sent or checked.

### `doc.go`
The package comment.

## Where state lives, and why Postgres only
**Nothing in memory.** There's no `store.go`, no write-through and no reconcile. Each piece of state lives where it's needed:

| State | Lives in | For how long |
|---|---|---|
| `{state, verifier, next}` | The browser's `labyrinth_oauth_state` cookie, signed | **10 m** (`auth.oauth_state_ttl`), or until the callback clears it |
| Login code | `auth_code` in Postgres (hash only) | Until redeemed, or **2 m** (`auth.login_code_ttl`); the row is swept within a further **10 m** (`auth.login_code_sweep`) |
| Access token | SvelteKit's `labyrinth_token` cookie; nothing server-side | **12 h** (`auth.token_ttl`) |
| Signing key | `JWT_PRIVATE_KEY` in `.env` | Until rotated |

**Why Postgres only:** a sign-in happens about once per user per **12 h** (`auth.token_ttl`), so there's nothing hot to cache. Verifying a token on `/api/*` touches no state at all. Keeping login codes in Postgres (not memory) means a restart between callback and redemption loses nothing, and codes need no lock in Go: the `DELETE … RETURNING` row lock, held until the redemption's transaction ends, makes them single-use.

## Data model: Postgres
Migration `0001_users.sql`. `auth` writes only `auth_code`; it reads and writes `userprofile` only through the `user` use case (see `internal/user/README.md`).

### `auth_code`
| Column | Type | Constraint | Rule it backs |
|---|---|---|---|
| `code_hash` | `BYTEA` | `PRIMARY KEY` | SHA-256 of the login code. A database leak doesn't reveal redeemable codes. The key makes lookup by hash one index probe; two codes colliding would fail the insert (with 256-bit codes this doesn't happen). |
| `user_id` | `UUID` | `NOT NULL REFERENCES userprofile (id) ON DELETE CASCADE` | A code belongs to one existing user. Deleting the user deletes their codes. |
| `expires_at` | `TIMESTAMPTZ` | `NOT NULL` | Set by Postgres to `now()` + **2 m** (`auth.login_code_ttl`). Redeemable while `expires_at > now()`; swept when `expires_at <= now()`. |
| `created_at` | `TIMESTAMPTZ` | `NOT NULL DEFAULT now()` | Audit only; not read by code. |

Index `auth_code_expires_at_idx` on `(expires_at)` backs the sweep.

No `used_at` column: redeeming **deletes** the row, which is what makes reuse impossible. The delete commits only together with a successful user load and token signature.

## Walkthroughs
`PUBLIC_URL=http://localhost:3000`, `FRONTEND_URL=http://localhost:5173`, `ALLOWED_EMAIL_DOMAINS=snu.edu.in`, `config.toml` as shipped. Google account: `sub 109876543210987654321`, `asha.rao@snu.edu.in`, verified, name `Asha Rao`, picture `https://lh3.googleusercontent.com/a/ACg8ocK1`. Times are 2026-10-17, UTC.

### 1. First sign-in, end to end
| Time | Request | Check | Postgres after | Response |
|---|---|---|---|---|
| 10:00:00 | Browser `GET /auth/google/login?next=%2Fteam` | `next=/team` is a same-site path: kept | unchanged | `302` to Google (`state=l_JRQysJ3emPfW5hF3X1KGFjXgMuq5vs8u9WLMz44N4`, `code_challenge=11-3oOow8RAY4BLiC2lSraZ3j4Oo0ROD9paA18F0nak`); state cookie with `exp` 10:10:00 (1792231800) |
| 10:00:03 | Browser `GET /auth/google/callback?state=l_JRQ…44N4&code=4/0AdQt8qh…` + cookie | cookie verifies; states equal; no `error`; code present; Google exchange with verifier `bx0jR6WHq2ivmEkogrVVAKUbBrMFp7tyedWNc4L8YtU` OK; email verified; domain `snu.edu.in` allowed | `userprofile` +1 (`id 3f2a9c4e-1b7d-4e8a-9c51-0d6b2e7f8a14`, role `PLAYER`); `auth_code` +1 (`code_hash \x34d818c8…94cf`, `expires_at` 10:02:03) | `302` to `http://localhost:5173/auth/callback?code=OFHvhOSp16AJpSTpguPJEvDmsoB48YcLon779lfjWtY&next=%2Fteam`; state cookie cleared |
| 10:00:04 | SvelteKit `POST /auth/token {"code":"OFHv…WtY"}` | hash `34d8…94cf` found, `expires_at` 10:02:03 > now | `auth_code` −1 | `200`, token with `iat` 1792231204, `exp` 1792274404; `expires_at` 22:00:04, the same second |
| 10:00:04 | SvelteKit `POST /auth/token` again, same code (a retry) | no row | unchanged | `400 invalid_code` |
| 10:00:05 | SvelteKit `GET /api/me`, `Bearer` token | `RequireAuth`: EdDSA, issuer, audience, `exp` 22:00:04 > now, `sub` is a UUID | unchanged | `200` user |
| 22:00:04 | SvelteKit `GET /api/me`, same token | `now < exp` fails (the limit is exclusive) | unchanged | `401 invalid_token`. SvelteKit's own cookie also expired at 22:00:04, so the user signs in again |

### 2. The login code expires before redemption
| Time | Event | Result |
|---|---|---|
| 10:00:03 | Callback stores the code; Postgres sets `expires_at` = its `now()` + 2 m = 10:02:03 | `302` to `/auth/callback?code=…` |
| 10:02:03.000 | `POST /auth/token` | `expires_at > now()` is false at exactly the expiry: `400 invalid_code` |
| 10:02:02.999 | (instead) `POST /auth/token` | redeemed: `200` |
| 10:10:00 | Sweep tick (every **10 m**, `auth.login_code_sweep`, counted from backend start) | Row deleted if still there |

### 3. The user cancels on Google's screen
| Request | Check | Response |
|---|---|---|
| `GET /auth/google/callback?state=l_JRQ…44N4&error=access_denied` + cookie | cookie valid, state matches, `error` set | `302` to `http://localhost:5173/auth/error?reason=google_access_denied`; no user or code written |

### 4. A forged callback (CSRF attempt)
An attacker sends the victim to `/auth/google/callback?state=forged&code=<attacker's Google code>`.

| Victim's browser has | Response |
|---|---|
| No state cookie (didn't start a sign-in, or it was more than **10 m** ago: `auth.oauth_state_ttl`) | `reason=state_missing` |
| A state cookie from its own sign-in | `state` ≠ cookie's: `reason=state_mismatch`. The attacker's code is never exchanged |

### 5. Two tabs
Tab A opens `/auth/google/login` at 10:00:00 (state `S1`, cookie `C1`). Tab B opens it at 10:00:10 (state `S2`, cookie `C2` replaces `C1`). Tab B finishes first: `S2` matches `C2`, success, cookie cleared. Tab A then returns with `S1`: no cookie, `reason=state_missing`. If tab A returns before tab B: `S1` ≠ `C2`, `reason=state_mismatch`, cookie cleared, and tab B then fails with `state_missing`. Only one sign-in per browser can be in flight.

### 6. Open-redirect attempt
| `next` sent to `/auth/google/login` | Stored in cookie, and sent to `/auth/callback` |
|---|---|
| `/team?tab=1#x` | `/team?tab=1#x` |
| `//evil.com` | `/` |
| `/\evil.com` | `/` |
| `https://evil.com` | `/` |
| `javascript:alert(1)` | `/` |
| `/ok\r\nSet-Cookie:` | `/` |
| missing | `/` |

### 7. Redemption fails after the code is found
| Time | Request | Inside the transaction | Postgres after | Response |
|---|---|---|---|---|
| 10:00:04 | `POST /auth/token {"code":"OFHv…WtY"}`; Postgres drops the connection while loading the user | `DELETE … RETURNING` finds the row; `users.Get` fails | rolled back: `auth_code` row still there, `expires_at` 10:02:03 | `500 internal error` (logged `redeem login code`) |
| 10:00:05 | SvelteKit retries with the same code | row found and deleted; user loaded; token signed; commit | `auth_code` −1 | `200` |
| 10:00:05 | (instead) The user was deleted after the callback | `ON DELETE CASCADE` removed the code with the user: no row | unchanged | `400 invalid_code` |

### 8. The same address on a second Google account
`asha.rao@snu.edu.in` is already user `3f2a…8a14` (Google `sub 109876543210987654321`). Asha signs in with another Google account, `sub 200000000000000000001`, that also reports `asha.rao@snu.edu.in` (an address moved between accounts).

| Step | Result |
|---|---|
| `INSERT INTO userprofile …` for the new `sub` | Violates `userprofile_email_key`; `user` returns `ErrEmailInUse` |
| Callback | `302` to `/auth/error?reason=email_in_use`; no user, no code; the hook isn't called |

## API
Every JSON response uses the envelope from `httpx`: `{"success", "code", "message", "payload"}`, with empty fields omitted. Auth's expected errors carry a stable `code` (`invalid_body`, `invalid_code`, `user_not_found`, `missing_token`, `invalid_token`); only `500 internal error` has none. Responses carry `Cache-Control: no-store`. Request bodies over **1 MiB** (`server.max_body_bytes`) fail to decode. A wrong method on a registered path (e.g. `GET /auth/token`) gets the server's `405 method_not_allowed` envelope with an `Allow` header (see `internal/server/README.md`).

| Method | Path | Auth | Body | Success |
|---|---|---|---|---|
| GET | `/auth/google/login?next=/path` | none | none | `302` to Google, sets state cookie |
| GET | `/auth/google/callback?state&code` or `?state&error` | state cookie | none | `302` to `FRONTEND_URL/auth/callback?code&next` |
| POST | `/auth/token` | none (server to server) | `{"code": "…"}` | `200` `tokenResponse` |
| every `/api/*` | | `Authorization: Bearer <jwt>` | | the route's own response |

### `GET /auth/google/login`
Request:
```
GET /auth/google/login?next=%2Fteam HTTP/1.1
Host: localhost:3000
```
Response:
```
HTTP/1.1 302 Found
Location: https://accounts.google.com/o/oauth2/auth?client_id=1234567890-abc.apps.googleusercontent.com&code_challenge=11-3oOow8RAY4BLiC2lSraZ3j4Oo0ROD9paA18F0nak&code_challenge_method=S256&prompt=select_account&redirect_uri=http%3A%2F%2Flocalhost%3A3000%2Fauth%2Fgoogle%2Fcallback&response_type=code&scope=openid+email+profile&state=l_JRQysJ3emPfW5hF3X1KGFjXgMuq5vs8u9WLMz44N4
Set-Cookie: labyrinth_oauth_state=eyJhbGciOiJFZERTQSIsInR5cCI6IkpXVCJ9.<payload>.<signature>; Path=/auth/google; Max-Age=600; HttpOnly; SameSite=Lax
```
With an `https` `PUBLIC_URL`, the cookie also has `Secure`.

The state cookie's JWT header is `{"alg":"EdDSA","typ":"JWT"}`, and its payload:
```json
{
  "state": "l_JRQysJ3emPfW5hF3X1KGFjXgMuq5vs8u9WLMz44N4",
  "verifier": "bx0jR6WHq2ivmEkogrVVAKUbBrMFp7tyedWNc4L8YtU",
  "next": "/team",
  "iss": "labyrinth-backend",
  "aud": ["labyrinth-oauth-state"],
  "exp": 1792231800,
  "iat": 1792231200
}
```
`exp` − `iat` = **600 s** (`auth.oauth_state_ttl`). There's no `sub`.

On failure (random source or signing fails; both logged): `500` with
```json
{"success": false, "message": "internal error"}
```

### `GET /auth/google/callback`
Request (from Google, via the browser):
```
GET /auth/google/callback?state=l_JRQysJ3emPfW5hF3X1KGFjXgMuq5vs8u9WLMz44N4&code=4%2F0AdQt8qhXyZ&scope=email+profile+openid HTTP/1.1
Host: localhost:3000
Cookie: labyrinth_oauth_state=eyJhbGciOiJFZERTQSIsInR5cCI6IkpXVCJ9.<payload>.<signature>
```
Success:
```
HTTP/1.1 302 Found
Location: http://localhost:5173/auth/callback?code=OFHvhOSp16AJpSTpguPJEvDmsoB48YcLon779lfjWtY&next=%2Fteam
Set-Cookie: labyrinth_oauth_state=; Path=/auth/google; Max-Age=0; HttpOnly; SameSite=Lax
```
Failure (every case, see [the table](#get-authgooglecallback)):
```
HTTP/1.1 302 Found
Location: http://localhost:5173/auth/error?reason=state_mismatch
Set-Cookie: labyrinth_oauth_state=; Path=/auth/google; Max-Age=0; HttpOnly; SameSite=Lax
```
The callback never answers with JSON.

The SvelteKit side adds two reasons of its own on `/auth/error`: `code_missing` (its `/auth/callback` got no `code`) and `token_exchange_failed` (`POST /auth/token` didn't return `200`).

### `POST /auth/token`
Request (SvelteKit server → backend):
```
POST /auth/token HTTP/1.1
Host: localhost:3000
Content-Type: application/json

{"code": "OFHvhOSp16AJpSTpguPJEvDmsoB48YcLon779lfjWtY"}
```
`code`: the login code from the callback redirect. Required, non-empty. No other fields are allowed.

`200`:
```json
{
  "success": true,
  "payload": {
    "access_token": "eyJhbGciOiJFZERTQSIsInR5cCI6IkpXVCJ9.<payload>.<signature>",
    "token_type": "Bearer",
    "expires_at": "2026-10-17T22:00:04Z",
    "user": {
      "id": "3f2a9c4e-1b7d-4e8a-9c51-0d6b2e7f8a14",
      "email": "asha.rao@snu.edu.in",
      "name": "Asha Rao",
      "avatar_url": "https://lh3.googleusercontent.com/a/ACg8ocK1",
      "role": "PLAYER",
      "created_at": "2026-10-17T10:00:03.204117Z"
    }
  }
}
```
| Field | Meaning |
|---|---|
| `access_token` | The JWT. Send as `Authorization: Bearer <access_token>`. |
| `token_type` | Always `"Bearer"`. |
| `expires_at` | Issue time, truncated to the whole second, + **12 h** (`auth.token_ttl`), in the backend's local time zone. Exactly the JWT's `exp`. SvelteKit uses it as the cookie's `Expires`. |
| `user` | The user as in `GET /api/me` (see `internal/user/README.md`), read from Postgres at redemption. |

The access token's payload:
```json
{
  "email": "asha.rao@snu.edu.in",
  "name": "Asha Rao",
  "avatar_url": "https://lh3.googleusercontent.com/a/ACg8ocK1",
  "role": "PLAYER",
  "iss": "labyrinth-backend",
  "sub": "3f2a9c4e-1b7d-4e8a-9c51-0d6b2e7f8a14",
  "aud": ["labyrinth-api"],
  "exp": 1792274404,
  "iat": 1792231204
}
```
`avatar_url` is `null` when the user has none. `name` may be `""`.

`400`, bad body:
```json
{"success": false, "code": "invalid_body", "message": "body must be {\"code\": string}"}
```
`400`, unknown, expired or used code:
```json
{"success": false, "code": "invalid_code", "message": "invalid or expired code"}
```
`404`, the code's user was deleted (the code is kept):
```json
{"success": false, "code": "user_not_found", "message": "the code's user no longer exists"}
```
`500` (database error, or signing failed; the code is kept):
```json
{"success": false, "message": "internal error"}
```

### `RequireAuth` on `/api/*`
Request:
```
GET /api/me HTTP/1.1
Authorization: Bearer eyJhbGciOiJFZERTQSIsInR5cCI6IkpXVCJ9.<payload>.<signature>
```
`401`, header missing or malformed:
```
HTTP/1.1 401 Unauthorized
WWW-Authenticate: Bearer

{"success": false, "code": "missing_token", "message": "missing bearer token"}
```
`401`, token rejected:
```
HTTP/1.1 401 Unauthorized
WWW-Authenticate: Bearer error="invalid_token"

{"success": false, "code": "invalid_token", "message": "invalid or expired token"}
```

**Live events:** none. Auth publishes nothing.

## Rules and cases

### `GET /auth/google/login`
| Case | Result |
|---|---|
| Random source fails | `500 internal error` (logged) |
| Signing the state fails | `500 internal error` (logged) |
| `next` is a same-site path, e.g. `/team?tab=1#x` | Kept in the cookie; returned in the success redirect |
| `next` missing, empty, relative (`team`), absolute (`https://…`), protocol-relative (`//evil.com`), `/\evil.com`, containing `\r` `\n` `\t` or `\`, unparseable, or with a scheme or host | `/` |
| A state cookie already exists (second tab) | Overwritten; the earlier tab's callback will fail (walkthrough 5) |
| Otherwise | `302` to Google with S256 PKCE and `prompt=select_account`; cookie set for **600 s** (`auth.oauth_state_ttl`) |

### `GET /auth/google/callback`
Checked in this order; the first failure wins. The state cookie is cleared in **every** case. Every failure is `302` to `FRONTEND_URL/auth/error?reason=<reason>` and logged at warn.

| # | Case | `reason` |
|---|---|---|
| 1 | No `labyrinth_oauth_state` cookie (never set, expired after **10 m** `auth.oauth_state_ttl`, cleared by an earlier callback, other browser) | `state_missing` |
| 2 | Cookie isn't a valid state JWT: tampered, signed by another key (key rotated), not EdDSA, wrong issuer, audience not `labyrinth-oauth-state` (e.g. an access token), no `exp`, or expired | `state_invalid` |
| 3 | `state` parameter missing or ≠ the cookie's | `state_mismatch` |
| 4 | Google sent `error=<e>` (e.g. `access_denied` on Cancel) | `google_<e>`, e.g. `google_access_denied` |
| 5 | `code` missing or empty | `code_missing` |
| 6 | Token request fails (bad or reused code, wrong verifier, Google unreachable), no `id_token` in the response, or the ID token fails verification (signature, issuer, audience, expiry) or claim decoding | `google_exchange_failed` |
| 7 | `email_verified` false, or `email` empty | `email_unverified` |
| 8 | `ALLOWED_EMAIL_DOMAINS` set and the email's domain (lowercased, exact match) isn't in it | `email_domain_not_allowed` |
| 9 | The email belongs to another Google account's user (`user.ErrEmailInUse`, walkthrough 8) | `email_in_use` |
| 10 | `SignInWithGoogle` fails otherwise (database error) | `internal_error` |
| 11 | Storing the login code fails | `internal_error`; the user row from step 10 stays written and the hook has already refreshed teams |
| — | Success | Hook called (teams shows the new name and avatar at once); `302` to `FRONTEND_URL/auth/callback?code=<43 chars>&next=<SafeNext'd next>` |

### `POST /auth/token`
Checked in this order:

| Case | Result |
|---|---|
| Body empty, not JSON, `null`, not an object, `code` not a string, unknown fields, or over **1 MiB** (`server.max_body_bytes`) | `400 invalid_body` |
| `code` is `""` | `400 invalid_body` |
| No row with this code's hash (never issued, already redeemed, or swept) | `400 invalid_code` |
| Row exists but `expires_at <= now()` (exactly at **2 m** `auth.login_code_ttl` and after, by Postgres's clock) | `400 invalid_code`; the row stays until the sweep |
| Two requests redeem one code at once | Exactly one `200`; the others `400 invalid_code` |
| Database error during the delete | `500 internal error` |
| Code found, but `users.Get` finds no user | `404 user_not_found`; rolled back, code kept. A backstop: deleting a user cascades to their codes, so normally the code is gone first (`400 invalid_code`) |
| Code found, loading the user fails (database error) | `500 internal error`; rolled back, **code kept**: a retry within **2 m** (`auth.login_code_ttl`) can still succeed |
| Code found, signing fails | `500 internal error`; code kept |
| Commit fails | `500 internal error`; nothing was redeemed |
| Otherwise | `200 tokenResponse`; token valid **12 h** (`auth.token_ttl`) with the user's **current** role; code used up |

### `RequireAuth`
Checked in this order:

| Case | Result |
|---|---|
| No `Authorization` header, no space in it (`Bearer`), scheme not `Bearer` in any case (`Basic abc`), or nothing after the space | `401 missing_token`, `WWW-Authenticate: Bearer` |
| `bearer <jwt>` or `BEARER <jwt>` | Accepted (scheme is case-insensitive) |
| `Bearer  <jwt>` (two spaces) | The token is `" <jwt>"`, which doesn't parse: `401 invalid_token` |
| Not a JWT, bad signature, signed by another key | `401 invalid_token`, `WWW-Authenticate: Bearer error="invalid_token"` |
| `alg` not `EdDSA` (`none`, `HS256` keyed with the public key, `RS256`) | same `401` |
| `iss` ≠ `labyrinth-backend` | same `401` |
| `aud` lacks `labyrinth-api` (e.g. a state cookie used as a token) | same `401` |
| No `exp` | same `401` |
| `now >= exp` (exactly **12 h** `auth.token_ttl` after issue, and later) | same `401` |
| `nbf` present and in the future | same `401` (the backend never sets `nbf`) |
| `sub` not a UUID | same `401` |
| Token valid, user since deleted | Passes; the handler decides (`/api/me` returns `401 user_not_found`: the token's user is gone, an auth failure. Compare `POST /auth/token`, where a code's deleted user is `404 user_not_found`) |
| Token valid, role changed since sign-in | Passes with the **old** role in the identity |
| Otherwise | Identity on the context; handler runs |

### General rules
- The JWT never appears in a URL; only the single-use login code does.
- A token's `email`, `name`, `avatar_url` and `role` are a snapshot from redemption. A change applies at the next sign-in, at most **12 h** (`auth.token_ttl`) later. Handlers that need the current role must read it from `user.Get`, as `/api/me` does.
- Logout is SvelteKit deleting `labyrinth_token`. To invalidate every token and every in-flight state cookie at once, rotate the keypair (`go run . keygen`), update both `.env` files, and restart both servers.

## Concurrency
There are no locks and no in-memory state. Everything concurrent is settled by Postgres or by Google. A redemption holds one transaction (and one pool connection) open while it loads the user and signs the token: a few milliseconds.

| Race | Who wins |
|---|---|
| Two `POST /auth/token` with the same code | The first `DELETE … RETURNING` to lock the row. The other waits on that row lock until the first transaction ends: if it committed, the other finds no row (`400 invalid_code`); if it rolled back (user load or signing failed), the other redeems the code. Exactly one succeeds (`TestConcurrentRedemptionOnce`). |
| The sweep and a redemption | Disjoint: redemption needs `expires_at > now()`, the sweep `expires_at <= now()`. |
| The same callback URL replayed (refresh, or a copied cookie) | The first clears the cookie; a replay without it gets `state_missing`. With a copied cookie, Google refuses the reused Google code: `google_exchange_failed`. |
| Two tabs signing in in one browser | The later login overwrites the cookie; only one finishes (walkthrough 5). |
| Two first sign-ins of the same Google account at once | `INSERT … ON CONFLICT (google_sub)`: one row, both get the same user ID (see `internal/user/README.md`). |
| A sign-in and a team write for the same user | The hook takes the team's entry lock like any team write; whichever goes second sees the other's result. |
| A role change during a session | Not seen by existing tokens until they expire; seen by `/api/me` at once. |

`Tokens` and `Google` are immutable after construction and safe to share. Google's signing keys are fetched and cached by `oidc.RemoteKeySet`, which is safe for concurrent use.

## Failure and recovery
| Case | Result |
|---|---|
| Postgres down during the callback | `SignInWithGoogle` or `Create` fails: `reason=internal_error`. Nothing half-done matters: a user upserted without a code is just a user. |
| Postgres down during `POST /auth/token` | `500`. Before the commit, nothing is used up and a retry can succeed. If the commit happened but the response was lost, the code is gone; SvelteKit shows `token_exchange_failed` and the user signs in again. |
| Postgres down for `/api/*` | `RequireAuth` still works (no database). The handler behind it may fail. |
| Google unreachable | `/auth/google/login` still redirects (no call to Google). The callback fails with `google_exchange_failed`. The backend starts without Google: keys are fetched on the first ID-token verification. |
| Google rotates its signing keys | `oidc.RemoteKeySet` refetches when it sees an unknown key ID. |
| Backend restarts | Nothing to rebuild. Tokens stay valid (same key). Login codes survive in Postgres. A sign-in in progress continues if its cookie is still valid. The sweep timer restarts, so the first sweep is **10 m** (`auth.login_code_sweep`) after start. |
| Sweep fails | Logged at warn (unless shutting down) and retried at the next tick. Expired codes can't be redeemed in the meantime anyway. |
| `JWT_PRIVATE_KEY` rotated | Every access token gets `401 invalid_token`; every in-flight state cookie gets `state_invalid`. SvelteKit's hook deletes cookies it can't verify once it has the new public key. |
| `JWT_PUBLIC_KEY` on SvelteKit doesn't match | SvelteKit can't verify any token; every user looks signed out. |
| `auth.token_ttl` changed between restarts | New tokens get the new lifetime; tokens already issued keep their `exp`. |
| `auth.login_code_ttl` or `auth.oauth_state_ttl` changed | Applies to codes and cookies issued after the restart. |
| Backend and Postgres clocks disagree | No effect on login codes: `expires_at` is set and checked by Postgres's clock. Token times use the backend's clock, as SvelteKit's checks do with its own. |

## Limits and how to change them
| Limit | Where | To change |
|---|---|---|
| Access token lifetime **12 h** | `config.toml` `auth.token_ttl` (> 0) | Edit and restart. Also the longest a role change can take to reach tokens. |
| Login code lifetime **2 m** | `config.toml` `auth.login_code_ttl` (> 0) | Edit and restart. |
| Time on Google's screen **10 m** | `config.toml` `auth.oauth_state_ttl` (≥ 1 s, so the cookie's whole-second `Max-Age` is never 0) | Edit and restart. Used for both the cookie's `Max-Age` (whole seconds) and the JWT's `exp`. |
| Sweep interval **10 m** | `config.toml` `auth.login_code_sweep` (> 0) | Edit and restart. |
| Request body **1 MiB** | `config.toml` `server.max_body_bytes` | Edit and restart. Applies to every route. |
| State and login code size, 256 bits | `tokenBytes` in `write.go` | A security parameter; don't lower it. |
| PKCE verifier, 32 bytes | `oauth2.GenerateVerifier` | Fixed by the library (RFC 7636). |
| Allowed email domains | `.env` `ALLOWED_EMAIL_DOMAINS`, comma-separated; empty allows all | Edit and restart. |
| Issuer, audiences, algorithm | Constants in `read.go` | Change together with `frontend/src/lib/server/session.ts`, which checks the same issuer, audience and algorithm. |

## Not decided yet
- **⚠ No revocation.** A stolen access token works for up to **12 h** (`auth.token_ttl`). Logout only drops SvelteKit's cookie. The only kill switch is rotating the keypair, which signs everyone out.
- **⚠ Stale roles in tokens.** `identity.Role` is the role at redemption. No route gates on it yet. Any future admin or helper route must read the role from `user.Get`, or tokens must get much shorter.
- **⚠ `POST /auth/token` is public.** It's meant to be server to server, but anyone holding a code within **2 m** (`auth.login_code_ttl`) can redeem it. Codes are 256-bit, so guessing is infeasible, and there's no rate limit.
- **`hd` isn't read.** The domain rule uses the email's domain, exact match only. Subdomains aren't allowed unless listed. Whether to require Google's `hd` claim instead is open.
- **No OIDC nonce.** The flow relies on state, PKCE and the back-channel code exchange. Adding a nonce would mean storing it in the state cookie.
- **The state cookie is signed, not encrypted.** Its payload (state, verifier, next) is readable by whoever holds the cookie, i.e. the browser that started the sign-in.

## Tests
`tokens_test.go` (no database):

| Test | Covers |
|---|---|
| `TestSafeNext` | `""`, `/`, `/team`, `/team?tab=1#x` kept; `//evil.com`, `//evil.com/path`, `/\evil.com`, `https://evil.com`, `evil.com`, `javascript:alert(1)`, `/ok\r\nSet-Cookie:` → `/` |
| `TestAccessTokenRoundTrip` | issue then verify: same user ID, email, role; expiry about TTL away |
| `TestAccessTokenRejections` | subtests: `wrong key`, `expired`, `alg none`, `HS256 with public key as secret`, `state token used as access token`, `access token used as state` |

`codes_test.go` (real Postgres, schema `auth_test`, `config.toml`'s `auth.login_code_ttl`):

| Test | Covers |
|---|---|
| `TestFailedRedemptionKeepsCode` | `use` failing returns its error and keeps the code; the retry succeeds with the right user; a third redemption → `ErrInvalidCode` (the test checks the wrapped `postgres.ErrNotFound`) |
| `TestConcurrentRedemptionOnce` | 8 concurrent redemptions of one code: exactly one succeeds, the rest get `ErrInvalidCode` (checked as the wrapped `postgres.ErrNotFound`) |
| `TestCodeExpiryUsesDatabaseClock` | `expires_at − now()` in Postgres is within 5 s under `auth.login_code_ttl` |

`internal/server/auth_test.go` (real Postgres, fake Google issuing RS256 ID tokens, `config.toml`'s settings):

| Test | Covers |
|---|---|
| `TestFullLoginFlow` | login with PKCE S256, client ID and redirect URI; one `HttpOnly` cookie; callback → `/auth/callback?code&next=/team`; redemption payload; **code reuse → `400`**; `/api/me`; a role change seen by `/api/me` with the old token; second sign-in keeps ID and role, refreshes name |
| `TestCallbackFailures` | subtests: `no state cookie` → `state_missing`; `state mismatch`; `tampered cookie` → `state_invalid`; `user cancelled` → `google_access_denied`; `bad code` → `google_exchange_failed`; `unverified email` → `email_unverified`; `domain not allowed` → `email_domain_not_allowed`; `open redirect in next` → `next=/` |
| `TestTokenAndAuthErrors` | expired code and unknown code → `400 invalid_code`; empty code → `400 invalid_body`; `DeleteExpired` removes exactly the expired one; `/api/me` with no token → `401 missing_token`, with `not-a-jwt` → `401 invalid_token`; `/healthz`; `POST /api/me` → `405` |
| `TestSignInRefreshesTeamProfile` | Signing in again with a new name and no picture: the team's member shows the new name at once and keeps the old avatar |
| `TestEmailInUse` | Another account already has the email: callback → `reason=email_in_use` |

`internal/server/events_test.go`: `TestEventStreamAuthAndCleanup` checks the event stream needs a token (`401`).

Not covered by any test yet: an expired state cookie, `code_missing`, `internal_error`, an empty email, `404 user_not_found` on `/auth/token`, a malformed (non-empty) body on `/auth/token`, the `Bearer` scheme's case and spacing, a missing `exp`, a non-UUID `sub`, the two `WWW-Authenticate` headers, and the cookie's `Secure` flag.
