package qr

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Clock is where the verifier gets the current time, so a token's freshness
// is judged against one clock. systemClock is the real one; tests pass a fake
// they set by hand.
type Clock interface {
	// Now returns the current time, in UTC truncated to microseconds.
	Now() time.Time
}

// systemClock is the real Clock.
type systemClock struct{}

// Now returns the current time in UTC, truncated to microseconds.
func (systemClock) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// Atlas is what the qr use case needs from the atlas use case: whether a
// waypoint ID exists, so a display can't be issued for a made-up waypoint.
// The atlas never changes after startup, so reading it needs no lock.
type Atlas interface {
	// WaypointIndex looks up a waypoint by its 4-digit ID.
	WaypointIndex(id string) (int, bool)
}

// Rules are the QR settings from config.toml.
type Rules struct {
	// Rotate is how often a waypoint's rotating QR changes (qr.rotate).
	Rotate time.Duration
	// Tolerance is how much longer than Rotate a scan stays accepted
	// (qr.tolerance). A token is valid for Rotate + Tolerance.
	Tolerance time.Duration
	// Clock is the verifier's time source; nil means the real one
	// (systemClock). Tests pass a fake. Not in config.toml.
	Clock Clock
}

// Verifier signs and verifies waypoint tokens. It holds the secret and the
// rules but no mutable state, so one instance is shared by every goroutine.
type Verifier struct {
	secret []byte
	rules  Rules
	atlas  Atlas
	clock  Clock
}

// New returns a verifier backed by secret (the QR_SECRET env var), rules and
// a (the atlas's waypoints).
func New(secret []byte, rules Rules, a Atlas) *Verifier {
	v := &Verifier{secret: secret, rules: rules, atlas: a, clock: rules.Clock}
	if v.clock == nil {
		v.clock = systemClock{}
	}
	return v
}

// now returns the verifier's current time.
func (v *Verifier) now() time.Time { return v.clock.Now() }

// window is how long a token is valid after issue: qr.rotate + qr.tolerance.
func (v *Verifier) window() time.Duration { return v.rules.Rotate + v.rules.Tolerance }

// keyFor derives the per-waypoint HMAC key: the waypoint's display signs with
// it, so a display for one waypoint can't mint tokens for another.
func (v *Verifier) keyFor(waypointID string) []byte {
	return sign(v.secret, "qr:"+waypointID)
}

// sign returns the HMAC-SHA256 of payload under key.
func sign(key []byte, payload string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// ErrInvalid means the token is malformed or its signature doesn't check out.
var ErrInvalid = errors.New("invalid qr token")

// ErrExpired means the token is well-formed and signed but no longer within
// its validity window (it was issued more than qr.rotate + qr.tolerance ago,
// or in the future).
var ErrExpired = errors.New("qr token expired")

// Verify checks token's signature and freshness and returns the waypoint ID it
// carries. ErrInvalid if the token can't be parsed or the signature is wrong;
// ErrExpired if it's outside the validity window. It doesn't check the waypoint
// exists: that's the caller's rule (teamstate's Scan checks the atlas).
func (v *Verifier) Verify(token string) (string, error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return "", ErrInvalid
	}
	waypoint, secs, ok := strings.Cut(payload, ":")
	if !ok || waypoint == "" {
		return "", ErrInvalid
	}
	issuedSecs, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return "", ErrInvalid
	}
	// Strict: a 32-byte signature is 43 characters whose last carries only 4
	// bits, so the lenient decoder would also accept the 3 spellings with
	// non-zero filler bits. Only the canonical encoding is a valid token.
	sigBytes, err := base64.RawURLEncoding.Strict().DecodeString(sig)
	if err != nil || !hmac.Equal(sign(v.keyFor(waypoint), payload), sigBytes) {
		return "", ErrInvalid
	}
	issued := time.Unix(issuedSecs, 0).UTC()
	now := v.now()
	if now.Before(issued) || !now.Before(issued.Add(v.window())) {
		return "", ErrExpired
	}
	return waypoint, nil
}
