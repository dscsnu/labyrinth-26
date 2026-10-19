package tests

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"
	"time"

	"labyrinth/internal/qr"
)

// fakeClock returns whatever time it was set to.
type fakeClock struct{ now time.Time }

// Now returns the set time.
func (f *fakeClock) Now() time.Time { return f.now }

// fakeAtlas knows a fixed set of waypoint IDs.
type fakeAtlas map[string]int

// WaypointIndex looks up id in the fake atlas.
func (f fakeAtlas) WaypointIndex(id string) (int, bool) {
	i, ok := f[id]
	return i, ok
}

// base is the fixed time tests sign tokens at, a whole second.
var base = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// newVerifier returns a verifier with a known secret and a fake clock set to
// base, knowing waypoints "1001" and "1002".
func newVerifier(secret []byte) (*qr.Verifier, *fakeClock) {
	c := &fakeClock{now: base}
	v := qr.New(secret, qr.Rules{Rotate: 5 * time.Second, Tolerance: 5 * time.Second, Clock: c},
		fakeAtlas{"1001": 0, "1002": 1})
	return v, c
}

// mint builds a token the way a display would: it signs waypoint:secs with
// the hex key, base64url-encoding the signature.
func mint(t *testing.T, keyHex, waypoint string, secs int64) string {
	t.Helper()
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		t.Fatal(err)
	}
	payload := waypoint + ":" + strconv.FormatInt(secs, 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// TestSigningKeyAndVerify checks the returned key signs tokens Verify accepts,
// and every boundary: tampering, a different key, malformed tokens, and the
// exact expiry edge.
func TestSigningKeyAndVerify(t *testing.T) {
	secret := bytes.Repeat([]byte{0x42}, 32)
	v, clock := newVerifier(secret)

	key, err := v.SigningKey("1001")
	if err != nil {
		t.Fatalf("signing key: %v", err)
	}
	if key.WaypointID != "1001" || key.RotateMs != 5000 || key.WindowMs != 10000 || key.ServerTime != base.Unix() {
		t.Fatalf("signing key %+v", key)
	}
	if len(key.Key) != 64 { // 32 bytes, hex
		t.Fatalf("key length %d, want 64", len(key.Key))
	}

	tok := mint(t, key.Key, "1001", base.Unix())

	// Within the window: accepted, and the waypoint comes back.
	clock.now = base.Add(3 * time.Second)
	if got, err := v.Verify(tok); err != nil || got != "1001" {
		t.Fatalf("verify at +3s: %q, %v", got, err)
	}
	clock.now = base.Add(9 * time.Second)
	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("verify at +9s: %v", err)
	}

	// Exactly at expiry, and past it: rejected as expired.
	for _, at := range []time.Time{base.Add(10 * time.Second), base.Add(11 * time.Second)} {
		clock.now = at
		if _, err := v.Verify(tok); !errors.Is(err, qr.ErrExpired) {
			t.Fatalf("verify at %v: %v, want ErrExpired", at, err)
		}
	}

	// A token from the future: rejected.
	clock.now = base.Add(-time.Second)
	if _, err := v.Verify(tok); !errors.Is(err, qr.ErrExpired) {
		t.Fatalf("verify in the past: %v, want ErrExpired", err)
	}

	// A different waypoint's key can't sign this one's token.
	key1002, _ := v.SigningKey("1002")
	clock.now = base.Add(3 * time.Second)
	if _, err := v.Verify(mint(t, key1002.Key, "1001", base.Unix())); !errors.Is(err, qr.ErrInvalid) {
		t.Fatalf("1001 token signed with 1002's key: %v, want ErrInvalid", err)
	}

	// A different secret's key can't sign a token this verifier accepts.
	other, otherClock := newVerifier(bytes.Repeat([]byte{0x24}, 32))
	otherKey, _ := other.SigningKey("1001")
	otherClock.now = base.Add(3 * time.Second)
	if _, err := v.Verify(mint(t, otherKey.Key, "1001", base.Unix())); !errors.Is(err, qr.ErrInvalid) {
		t.Fatalf("token signed with another secret: %v, want ErrInvalid", err)
	}

	// Malformed tokens: invalid.
	clock.now = base
	for _, bad := range []string{"", "notatoken", "1001:abc.<sig>", "1001:1.<sig>", "1001:1760000000.!base64"} {
		if _, err := v.Verify(bad); !errors.Is(err, qr.ErrInvalid) {
			t.Fatalf("malformed %q: %v, want ErrInvalid", bad, err)
		}
	}
}

// TestSigningKeyUnknownWaypoint checks a display can't be issued for a waypoint
// the atlas doesn't have.
func TestSigningKeyUnknownWaypoint(t *testing.T) {
	v, _ := newVerifier(bytes.Repeat([]byte{0x42}, 32))
	if _, err := v.SigningKey("9999"); !errors.Is(err, qr.ErrUnknownWaypoint) {
		t.Fatalf("signing key unknown waypoint: %v, want ErrUnknownWaypoint", err)
	}
}
