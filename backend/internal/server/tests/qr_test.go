package tests

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// qrToken builds a token the way a display would, for a given waypoint and
// issue second: it signs with the per-waypoint key derived from the test
// secret. Used to exercise expiry and tampering over HTTP without a fake clock
// on the server.
func qrToken(waypoint string, secs int64) string {
	key := hmac.New(sha256.New, qrTestSecret)
	key.Write([]byte("qr:" + waypoint))
	payload := waypoint + ":" + strconv.FormatInt(secs, 10)
	mac := hmac.New(sha256.New, key.Sum(nil))
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// TestQRGuard checks the mint route needs a token and a HELPER or ADMIN role
// as stored now (the token's role is ignored), that the ID must be 4 digits,
// and that an unknown waypoint is refused.
func TestQRGuard(t *testing.T) {
	e := setup(t)
	if err := e.db.SeedDev2025(context.Background()); err != nil {
		t.Fatal(err)
	}
	e = e.restart() // content is read at startup
	p := e.newPlayer("p")

	expect(t, "no token", e.call("GET", "/api/qr/1001", player{}, nil), http.StatusUnauthorized, "missing_token")
	expect(t, "player", e.call("GET", "/api/qr/1001", p, nil), http.StatusForbidden, "not_staff")
	expect(t, "bad id", e.call("GET", "/api/qr/10", p, nil), http.StatusForbidden, "not_staff") // role checked first

	e.setRole(p, "HELPER") // the token still says PLAYER
	expect(t, "bad id as helper", e.call("GET", "/api/qr/10", p, nil), http.StatusBadRequest, "invalid_waypoint_id")
	expect(t, "unknown waypoint", e.call("GET", "/api/qr/9999", p, nil), http.StatusNotFound, "unknown_waypoint")

	r := e.call("GET", "/api/qr/1001", p, nil)
	expect(t, "helper mints", r, http.StatusOK, "")
	if r.payload["waypoint_id"] != "1001" || r.payload["key"] == "" {
		t.Fatalf("mint payload: %v", r.payload)
	}

	e.setRole(p, "ADMIN")
	expect(t, "admin mints", e.call("GET", "/api/qr/1001", p, nil), http.StatusOK, "")

	e.setRole(p, "PLAYER")
	expect(t, "demoted", e.call("GET", "/api/qr/1001", p, nil), http.StatusForbidden, "not_staff")
}

// TestQRVerify checks POST /api/qr/verify over HTTP: a fresh token returns its
// waypoint, an expired one is qr_expired, a tampered one invalid_qr, and a
// missing body invalid_body.
func TestQRVerify(t *testing.T) {
	e := setup(t)
	p := e.newPlayer("p")
	verify := func(token string) apiResult {
		return e.call("POST", "/api/qr/verify", p, map[string]string{"qr": token})
	}

	fresh := qrToken("1001", time.Now().Unix())
	r := verify(fresh)
	expect(t, "fresh", r, http.StatusOK, "")
	if r.payload["waypoint_id"] != "1001" {
		t.Fatalf("verified waypoint: %v", r.payload)
	}

	expect(t, "expired", verify(qrToken("1001", time.Now().Unix()-100)), http.StatusConflict, "qr_expired")

	// Flip the last signature character to a different, still-valid base64url
	// one, so the signature can't match regardless of the real value.
	last := fresh[len(fresh)-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'B'
	}
	bad := fresh[:len(fresh)-1] + string(replacement)
	expect(t, "tampered", verify(bad), http.StatusBadRequest, "invalid_qr")

	expect(t, "empty body", e.call("POST", "/api/qr/verify", p, map[string]string{}), http.StatusBadRequest, "invalid_body")
}
