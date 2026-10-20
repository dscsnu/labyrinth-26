package tests

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/teamstate"
)

// qrStarted is started with config.toml's game.qr_cooldown on, skipping
// unless it's long enough for the tests' few seconds of scans (minQRCooldown).
func qrStarted(t *testing.T, o fixtureOptions) *fixtureEnv {
	t.Helper()
	o.qrCooldown = true
	f := started(t, o)
	if f.rules.QRCooldown < minQRCooldown {
		t.Skipf("test assumes game.qr_cooldown >= %v (got %v)", minQRCooldown, f.rules.QRCooldown)
	}
	return f
}

// minQRCooldown is the shortest game.qr_cooldown the QR cooldown tests work
// with: they scan for up to 20 s after a cast and expect its waypoints still
// cooling.
const minQRCooldown = 30 * time.Second

// qrIDs returns the waypoint IDs in v.QRCooldowns, in order, and fails unless
// every entry's until is until.
func qrIDs(t *testing.T, v teamstate.View, until time.Time) []string {
	t.Helper()
	var ids []string
	for _, c := range v.QRCooldowns {
		if !c.Until.Equal(until) {
			t.Fatalf("qr_cooldowns %+v: %s until %v, want %v", v.QRCooldowns, c.WaypointID, c.Until, until)
		}
		ids = append(ids, c.WaypointID)
	}
	return ids
}

// TestQRCooldown follows the square's waypoints through game.qr_cooldown:
//
//   - the cast puts its 4 waypoints in qr_cooldowns, until cast_at +
//     qr_cooldown, sorted by waypoint
//   - scanning any of them, by any member, is waypoint_on_cooldown: nothing
//     logged, pushed or published, the version unchanged
//   - a waypoint not in the square (1003) scans normally
//   - one microsecond before until, still refused
//   - at until, the timer applies the expiry with nobody acting: one
//     qr_cooldown_ended event, the version up by one, qr_cooldowns empty
//   - then the waypoints scan again and the triangle casts, cooling its own
//     3 waypoints from its cast
func TestQRCooldown(t *testing.T) {
	f := qrStarted(t, fixtureOptions{})
	sub := f.hub.Subscribe(f.users[1])
	defer sub.Close()
	cd := f.rules.QRCooldown

	if v := f.view(t); len(v.QRCooldowns) != 0 || v.QRCooldowns == nil {
		t.Fatalf("before any cast: qr_cooldowns %#v, want []", v.QRCooldowns)
	}
	res := f.castSquare(t, 0) // cast at 4 s
	for range 1 + len(square) {
		nextState(t, sub)
	}
	castAt := start.Add(f.tick(4))
	until := castAt.Add(cd)
	if !res.State.LastCast.CastAt.Equal(castAt) {
		t.Fatalf("cast at %v, want %v", res.State.LastCast.CastAt, castAt)
	}
	if got := qrIDs(t, res.State, until); !slices.Equal(got, square) {
		t.Fatalf("the cast's state: qr_cooldowns %v, want %v", got, square)
	}

	f.selectAt(t, f.tick(10), 0, "triangle")
	expectType(t, sub, "select", teamstate.ShapeSelected)
	seq, logged := f.view(t).Version.Seq, f.loggedScans(t)
	for i, w := range square {
		if err := f.scanErrAt(f.tick(11), i, w); !errors.Is(err, teamstate.ErrWaypointOnCooldown) {
			t.Fatalf("member %d scanning %s right after the square: %v, want waypoint_on_cooldown", i, w, err)
		}
	}
	expectNoEvent(t, sub, "refused scans")
	if v := f.view(t); v.Version.Seq != seq || len(v.Stack) != 0 || f.loggedScans(t) != logged {
		t.Fatalf("refused scans changed the state: seq %d (was %d), stack %v, %d scans logged (was %d)",
			v.Version.Seq, seq, v.Stack, f.loggedScans(t), logged)
	}
	expectOutcome(t, "1003, not in the square", f.scanAt(t, f.tick(12), 4, "1003"), teamstate.OutcomeScanAdded)
	expectType(t, sub, "1003", teamstate.ScanAdded)

	f.clock.at(until.Sub(start) - time.Microsecond)
	expectNoEvent(t, sub, "just before until")
	if err := f.scanErrAt(until.Sub(start)-time.Microsecond, 0, "1001"); !errors.Is(err, teamstate.ErrWaypointOnCooldown) {
		t.Fatalf("1001 a microsecond before until: %v, want waypoint_on_cooldown", err)
	}
	seq = f.view(t).Version.Seq

	f.clock.at(until.Sub(start))
	v := expectType(t, sub, "at until", teamstate.QRCooldownEnded)
	if v.Version.Seq != seq+1 || len(v.QRCooldowns) != 0 || len(v.Stack) != 1 {
		t.Fatalf("the expiry: seq %d (want %d), qr_cooldowns %v, stack %v", v.Version.Seq, seq+1, v.QRCooldowns, v.Stack)
	}
	expectNoEvent(t, sub, "one event per expiry")

	expectOutcome(t, "1001 at until", f.scanAt(t, until.Sub(start), 0, "1001"), teamstate.OutcomeScanAdded)
	res = f.scanAt(t, until.Sub(start)+f.tick(1), 1, "1002")
	expectOutcome(t, "the triangle", res, teamstate.OutcomePatternCompleted)
	if got := qrIDs(t, res.State, until.Add(f.tick(1)+cd)); !slices.Equal(got, triangle) {
		t.Fatalf("after the triangle: qr_cooldowns %v, want %v", got, triangle)
	}
}

// TestQRCooldownWriteCatchesUp checks that a scan never waits on a late
// timer: with the timers stopped, a scan of a square waypoint at until is
// preceded by a qr_cooldown_ended event and accepted.
func TestQRCooldownWriteCatchesUp(t *testing.T) {
	f := qrStarted(t, fixtureOptions{})
	sub := f.hub.Subscribe(f.users[0])
	defer sub.Close()
	f.castSquare(t, 0)
	f.selectAt(t, f.tick(10), 0, "triangle")
	for range 2 + len(square) {
		nextState(t, sub)
	}
	f.state.Close() // timers off: only writes catch up

	at := f.tick(4) + f.rules.QRCooldown
	f.clock.at(at)
	expectNoEvent(t, sub, "no timer")
	res := f.scanAt(t, at, 2, "1001")
	expectType(t, sub, "caught-up expiry", teamstate.QRCooldownEnded)
	expectType(t, sub, "the scan", teamstate.ScanAdded)
	if res.Outcome != teamstate.OutcomeScanAdded || len(res.State.QRCooldowns) != 0 {
		t.Fatalf("the scan: %s, qr_cooldowns %v", res.Outcome, res.State.QRCooldowns)
	}
}

// TestQRCooldownAndAttackTogether sets game.qr_cooldown to boss 2's attack
// interval, so the square (which defeats boss 1, starting boss 2's phase at
// the cast) has its waypoints open at the very moment boss 2 first attacks:
// one event, boss_attacked (the attack is the bigger news), carrying both
// changes.
func TestQRCooldownAndAttackTogether(t *testing.T) {
	f := started(t, fixtureOptions{attacks: true})
	f.rules.QRCooldown = f.interval(2) // known only once the bosses load

	f.restart(t)
	sub := f.hub.Subscribe(f.users[0])
	defer sub.Close()
	res := f.castSquare(t, 0)
	if !res.State.LastCast.DefeatedBoss {
		t.Fatalf("the square should defeat boss 1: %+v", res.State.LastCast)
	}
	for range 1 + len(square) {
		nextState(t, sub)
	}
	f.clock.at(f.tick(4) + f.interval(2))
	v := expectType(t, sub, "attack and expiry", teamstate.BossAttacked)
	if v.Boss.Attacks != 1 || len(v.QRCooldowns) != 0 {
		t.Fatalf("attack and expiry: boss %+v, qr_cooldowns %v", v.Boss, v.QRCooldowns)
	}
	expectNoEvent(t, sub, "one event for both")
}

// TestQRCooldownFailedCast checks that a completing scan that fails to
// persist starts no QR cooldown: with the database closed, the square's last
// scan fails and qr_cooldowns stays empty.
func TestQRCooldownFailedCast(t *testing.T) {
	f := qrStarted(t, fixtureOptions{})
	f.selectAt(t, 0, 0, "square")
	for i, w := range square[:3] {
		f.scanAt(t, f.tick(float64(i+1)), i, w)
	}
	f.db.Close()
	if err := f.scanErrAt(f.tick(4), 3, square[3]); err == nil {
		t.Fatal("the completing scan with the database closed should fail")
	}
	if v := f.view(t); len(v.QRCooldowns) != 0 || v.CastCount != 0 {
		t.Fatalf("after a failed cast: qr_cooldowns %v, casts %d", v.QRCooldowns, v.CastCount)
	}
}

// TestQRCooldownRestart checks that a restart rebuilds qr_cooldowns from the
// cast log: exactly while they're active (JSON equal), and without the ones
// that ended while the backend was down.
func TestQRCooldownRestart(t *testing.T) {
	f := qrStarted(t, fixtureOptions{})
	f.castSquare(t, 0)
	f.clock.at(f.tick(10))
	before := pgtest.Must(json.Marshal(f.view(t).QRCooldowns))
	f.restart(t)
	if after := pgtest.Must(json.Marshal(f.view(t).QRCooldowns)); string(after) != string(before) {
		t.Fatalf("qr_cooldowns across a restart\nbefore: %s\nafter:  %s", before, after)
	}
	if len(f.view(t).QRCooldowns) != len(square) {
		t.Fatalf("after a restart: qr_cooldowns %v", f.view(t).QRCooldowns)
	}

	f.state.Close() // down while the cooldown ends
	f.clock.at(f.tick(5) + f.rules.QRCooldown)
	f.restart(t)
	if v := f.view(t); len(v.QRCooldowns) != 0 {
		t.Fatalf("restarted after the cooldown ended: qr_cooldowns %v", v.QRCooldowns)
	}
	f.selectAt(t, f.tick(6)+f.rules.QRCooldown, 0, "triangle")
	expectOutcome(t, "1001 after the restart", f.scanAt(t, f.tick(7)+f.rules.QRCooldown, 0, "1001"), teamstate.OutcomeScanAdded)
}

// TestQRCooldownLatestCastWins checks the one way a waypoint can be in two
// active casts, game.qr_cooldown raised between restarts: with the square
// at 4 s and the triangle (sharing 1001 and 1002) one cooldown later, a
// restart with three times the cooldown shows 1001 and 1002 until the
// triangle's end, 1007 and 1008 until the square's, sorted by until.
func TestQRCooldownLatestCastWins(t *testing.T) {
	f := qrStarted(t, fixtureOptions{})
	cd := f.rules.QRCooldown
	f.castSquare(t, 0) // cast at 4 s
	f.selectAt(t, cd+f.tick(10), 0, "triangle")
	var res teamstate.ScanResult
	for i, w := range triangle {
		res = f.scanAt(t, cd+f.tick(float64(11+i)), i, w)
	}
	expectOutcome(t, "the triangle", res, teamstate.OutcomePatternCompleted)
	triangleAt := start.Add(cd + f.tick(13))

	f.rules.QRCooldown = 3 * cd
	f.restart(t)
	v := f.view(t)
	want := []teamstate.QRCooldownView{
		{WaypointID: "1007", Until: start.Add(f.tick(4) + 3*cd)},
		{WaypointID: "1008", Until: start.Add(f.tick(4) + 3*cd)},
		{WaypointID: "1001", Until: triangleAt.Add(3 * cd)},
		{WaypointID: "1002", Until: triangleAt.Add(3 * cd)},
		{WaypointID: "1003", Until: triangleAt.Add(3 * cd)},
	}
	if !slices.EqualFunc(v.QRCooldowns, want, func(a, b teamstate.QRCooldownView) bool {
		return a.WaypointID == b.WaypointID && a.Until.Equal(b.Until)
	}) {
		t.Fatalf("qr_cooldowns %+v\nwant %+v", v.QRCooldowns, want)
	}
}

// TestQRCooldownOff checks that game.qr_cooldown = 0 turns it off: after
// the square, qr_cooldowns is empty and its waypoints scan straight away.
func TestQRCooldownOff(t *testing.T) {
	f := started(t, fixtureOptions{rules: func(r *teamstate.Rules) { r.QRCooldown = 0 }})
	res := f.castSquare(t, 0)
	if res.State.QRCooldowns == nil || len(res.State.QRCooldowns) != 0 {
		t.Fatalf("qr_cooldowns with game.qr_cooldown 0: %#v, want []", res.State.QRCooldowns)
	}
	f.selectAt(t, f.tick(10), 0, "triangle")
	expectOutcome(t, "1001 straight after the square", f.scanAt(t, f.tick(11), 0, "1001"), teamstate.OutcomeScanAdded)
}
