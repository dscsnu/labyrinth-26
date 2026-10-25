package tests

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
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

// TestQRGuard checks the pick-any-waypoint route is for admins only, as
// stored now (the token's role is ignored): a player and a helper are
// 403 not_admin (helpers show their assigned checkpoint instead,
// TestQRMine); for an admin the ID must be 4 digits and the waypoint known.
func TestQRGuard(t *testing.T) {
	e := setup(t)
	if err := e.db.Seed2026(context.Background()); err != nil {
		t.Fatal(err)
	}
	e = e.restart() // content is read at startup
	p := e.newPlayer("p")

	expect(t, "no token", e.call("GET", "/api/qr/2001", player{}, nil), http.StatusUnauthorized, "missing_token")
	expect(t, "player", e.call("GET", "/api/qr/2001", p, nil), http.StatusForbidden, "not_admin")
	expect(t, "bad id", e.call("GET", "/api/qr/10", p, nil), http.StatusForbidden, "not_admin") // role checked first

	e.setRole(p, "HELPER") // the token still says PLAYER
	expect(t, "helper picks", e.call("GET", "/api/qr/2001", p, nil), http.StatusForbidden, "not_admin")

	e.setRole(p, "ADMIN")
	expect(t, "bad id as admin", e.call("GET", "/api/qr/10", p, nil), http.StatusBadRequest, "invalid_waypoint_id")
	expect(t, "unknown waypoint", e.call("GET", "/api/qr/9999", p, nil), http.StatusNotFound, "unknown_waypoint")
	r := e.call("GET", "/api/qr/2001", p, nil)
	expect(t, "admin mints", r, http.StatusOK, "")
	if r.payload["waypoint_id"] != "2001" || r.payload["key"] == "" {
		t.Fatalf("mint payload: %v", r.payload)
	}

	e.setRole(p, "PLAYER")
	expect(t, "demoted", e.call("GET", "/api/qr/2001", p, nil), http.StatusForbidden, "not_admin")
}

// TestQRMine checks GET /api/qr/me: staff only (a player is 403 not_staff);
// a helper with no checkpoint is 404 not_assigned; once an admin assigns
// one, the helper gets that waypoint's key (the same an admin gets for it),
// a reassignment switches it, and unassigning or demoting the helper ends
// it.
func TestQRMine(t *testing.T) {
	e := setup(t)
	if err := e.db.Seed2026(context.Background()); err != nil {
		t.Fatal(err)
	}
	e = e.restart()
	admin := e.newAdmin("admin")
	h := e.newPlayer("h")
	assign := func(waypoint any) apiResult {
		return e.call("PUT", "/api/admin/helpers/"+h.id+"/waypoint", admin, map[string]any{"waypoint_id": waypoint})
	}

	expect(t, "no token", e.call("GET", "/api/qr/me", player{}, nil), http.StatusUnauthorized, "missing_token")
	expect(t, "player", e.call("GET", "/api/qr/me", h, nil), http.StatusForbidden, "not_staff")
	expect(t, "add the helper", e.call("POST", "/api/admin/helpers", admin, map[string]string{"email": "h@snu.edu.in"}), http.StatusOK, "")
	expect(t, "unassigned", e.call("GET", "/api/qr/me", h, nil), http.StatusNotFound, "not_assigned")

	for _, w := range []string{"2003", "2007"} {
		expect(t, "assign "+w, assign(w), http.StatusOK, "")
		mine := e.call("GET", "/api/qr/me", h, nil)
		expect(t, "helper's key for "+w, mine, http.StatusOK, "")
		picked := e.call("GET", "/api/qr/"+w, admin, nil)
		if mine.payload["waypoint_id"] != w || mine.payload["key"] != picked.payload["key"] {
			t.Fatalf("%s: helper %v, admin %v", w, mine.payload, picked.payload)
		}
	}

	expect(t, "unassign", assign(nil), http.StatusOK, "")
	expect(t, "unassigned again", e.call("GET", "/api/qr/me", h, nil), http.StatusNotFound, "not_assigned")
	expect(t, "reassign", assign("2001"), http.StatusOK, "")
	expect(t, "demote", e.call("PUT", "/api/admin/users/"+h.id+"/role", admin, map[string]string{"role": "PLAYER"}), http.StatusOK, "")
	expect(t, "demoted", e.call("GET", "/api/qr/me", h, nil), http.StatusForbidden, "not_staff")
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

	fresh := qrToken("2001", time.Now().Unix())
	r := verify(fresh)
	expect(t, "fresh", r, http.StatusOK, "")
	if r.payload["waypoint_id"] != "2001" {
		t.Fatalf("verified waypoint: %v", r.payload)
	}

	expect(t, "expired", verify(qrToken("2001", time.Now().Unix()-100)), http.StatusConflict, "qr_expired")

	// Change the signature's first character to a different base64url one.
	// Every bit of a first character is signature data, so the bytes always
	// change and the signature can't match. (Changing the last character
	// doesn't always: it carries only 4 data bits; see qr's
	// TestVerifyRejectsNonCanonicalSignature.)
	dot := strings.IndexByte(fresh, '.')
	replacement := byte('A')
	if fresh[dot+1] == 'A' {
		replacement = 'B'
	}
	bad := fresh[:dot+1] + string(replacement) + fresh[dot+2:]
	expect(t, "tampered", verify(bad), http.StatusBadRequest, "invalid_qr")

	expect(t, "empty body", e.call("POST", "/api/qr/verify", p, map[string]string{}), http.StatusBadRequest, "invalid_body")
}
