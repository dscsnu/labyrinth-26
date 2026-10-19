// Package qr issues and verifies the short-lived, rotating waypoint tokens
// shown as QR codes. A staff member's /qr display fetches a waypoint-scoped
// signing key once and then signs its own token every qr.rotate (config.toml)
// seconds; a player scans it and submits it, and the backend accepts it only
// within qr.rotate + qr.tolerance of issue, so a screenshot shared later is
// already stale.
//
// It holds no state: every key and token is derived from the secret, the
// waypoint ID and the time, so there is no memory and no Postgres.
//
// See README.md for every rule and case, and how each file works.
package qr
