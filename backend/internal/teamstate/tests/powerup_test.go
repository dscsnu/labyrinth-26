package tests

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"

	"labyrinth/internal/bosses"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/teamstate"
)

// grant gives the fixture's team a powerup of kind and fails unless it was
// granted (not wasted).
func (f *fixtureEnv) grant(t *testing.T, kind teamstate.Powerup) teamstate.View {
	t.Helper()
	res, err := f.state.GrantPowerup(context.Background(), f.teamID, kind)
	if err != nil || !res.Granted {
		t.Fatalf("grant %s: granted %v, %v", kind.ID(), res.Granted, err)
	}
	return res.State
}

// activate has member i activate kind, failing on an error.
func (f *fixtureEnv) activate(t *testing.T, i int, kind teamstate.Powerup) teamstate.View {
	t.Helper()
	v, err := f.state.Activate(context.Background(), f.users[i], kind)
	if err != nil {
		t.Fatalf("activate %s: %v", kind.ID(), err)
	}
	return v
}

// active returns v's active powerups as "kind" or "kind@until" strings.
func active(v teamstate.View) []string {
	var out []string
	for _, a := range v.Powerups.Active {
		s := a.Kind
		if a.Until != nil {
			s += "@" + a.Until.Sub(start).String()
		}
		out = append(out, s)
	}
	return out
}

// TestGrantPowerup checks GrantPowerup: each kind is granted once
// (powerup_granted, held, version + 1); a second grant of a held kind is
// wasted (granted false, no event, same version); an unknown kind, an
// unknown team and a lobby team are refused.
func TestGrantPowerup(t *testing.T) {
	f := started(t)
	sub := f.hub.Subscribe(f.users[1])
	defer sub.Close()
	ctx := context.Background()

	var want []string
	for _, k := range []teamstate.Powerup{teamstate.PowerupDamageMultiplier, teamstate.PowerupHeart,
		teamstate.PowerupShortQRCooldown, teamstate.PowerupInstantCast} {
		seq := f.view(t).Version.Seq
		v := f.grant(t, k)
		want = append(want, k.ID())
		if ev := expectType(t, sub, "grant "+k.ID(), teamstate.PowerupGranted); ev.Version.Seq != seq+1 || !slices.Equal(ev.Powerups.Held, want) {
			t.Fatalf("grant %s: seq %d (was %d), held %v", k.ID(), ev.Version.Seq, seq, ev.Powerups.Held)
		}
		if !slices.Equal(v.Powerups.Held, want) {
			t.Fatalf("grant %s: the result's held %v", k.ID(), v.Powerups.Held)
		}
	}
	seq := f.view(t).Version.Seq
	res, err := f.state.GrantPowerup(ctx, f.teamID, teamstate.PowerupHeart)
	if err != nil || res.Granted || res.State.Version.Seq != seq {
		t.Fatalf("a second heart: granted %v, seq %d (was %d), %v", res.Granted, res.State.Version.Seq, seq, err)
	}
	expectNoEvent(t, sub, "a wasted grant")

	if _, err := f.state.GrantPowerup(ctx, f.teamID, teamstate.Powerup(9)); !errors.Is(err, teamstate.ErrUnknownPowerup) {
		t.Fatalf("an unknown kind: %v", err)
	}
	if _, err := f.state.GrantPowerup(ctx, "000000", teamstate.PowerupHeart); !errors.Is(err, teamstate.ErrTeamNotFound) {
		t.Fatalf("an unknown team: %v", err)
	}
	lobby := started(t, fixtureOptions{lobby: true})
	if _, err := lobby.state.GrantPowerup(ctx, lobby.teamID, teamstate.PowerupHeart); !errors.Is(err, teamstate.ErrNotInGame) {
		t.Fatalf("a lobby team: %v", err)
	}
	if id, ok := teamstate.PowerupByID("short_qr_cooldown"); !ok || id != teamstate.PowerupShortQRCooldown {
		t.Fatalf("PowerupByID: %v %v", id, ok)
	}
	if _, ok := teamstate.PowerupByID("shield"); ok {
		t.Fatal("PowerupByID found an unknown kind")
	}
}

// TestDamageMultiplierPowerup checks the damage multiplier: only the leader
// activates it, and only while held; it shows as active with no until, and
// survives a restart armed; activating it again while armed is
// powerup_active; the next cast deals damage × powerups.damage_multiplier,
// marked boosted, and uses it up; the cast after that is normal.
func TestDamageMultiplierPowerup(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	mult := teamstate.PowerupDamageMultiplier
	if _, err := f.state.Activate(ctx, f.users[0], mult); !errors.Is(err, teamstate.ErrPowerupNotHeld) {
		t.Fatalf("activating one not held: %v", err)
	}
	f.grant(t, mult)
	if _, err := f.state.Activate(ctx, f.users[1], mult); !errors.Is(err, teamstate.ErrNotLeader) {
		t.Fatalf("a member activating: %v", err)
	}
	if v := f.activate(t, 0, mult); !slices.Equal(active(v), []string{"damage_multiplier"}) || len(v.Powerups.Held) != 0 {
		t.Fatalf("after activation: active %v, held %v", active(v), v.Powerups.Held)
	}
	f.grant(t, mult)
	if _, err := f.state.Activate(ctx, f.users[0], mult); !errors.Is(err, teamstate.ErrPowerupActive) {
		t.Fatalf("activating while armed: %v", err)
	}
	f.restart(t)
	if v := f.view(t); !slices.Equal(active(v), []string{"damage_multiplier"}) || !slices.Equal(v.Powerups.Held, []string{"damage_multiplier"}) {
		t.Fatalf("after a restart: active %v, held %v", active(v), v.Powerups.Held)
	}

	res := f.castQuad(t, 0)
	want := int(math.Round(float64(f.damageOf(t, quad)) * f.rules.PowerupMultiplier))
	if lc := res.State.LastCast; lc.Dealt != want || !lc.Boosted || len(res.State.Powerups.Active) != 0 {
		t.Fatalf("the boosted cast: dealt %d (want %d), boosted %v, active %v", lc.Dealt, want, lc.Boosted, active(res.State))
	}
	if res := f.castTriangle(t, 1); res.State.LastCast.Boosted || res.State.LastCast.Dealt != f.damageOf(t, triangle) {
		t.Fatalf("the cast after: %+v", res.State.LastCast)
	}
	f.restart(t)
	if v := f.view(t); len(v.Powerups.Active) != 0 {
		t.Fatalf("used up, after a restart: active %v", active(v))
	}
}

// TestHeartPowerup checks the heart: refused at full health (full_health)
// and when knocked out; after boss 1's first attack it gives back
// powerups.heart health with attacks and next_attack_at unchanged, and a
// restart keeps it; a heart bigger than the damage taken stops at
// game.health.
func TestHeartPowerup(t *testing.T) {
	f := started(t, fixtureOptions{attacks: true, rules: func(r *teamstate.Rules) { r.PowerupHeart = 1 }})
	ctx := context.Background()
	heart := teamstate.PowerupHeart
	f.grant(t, heart)
	if _, err := f.state.Activate(ctx, f.users[0], heart); !errors.Is(err, teamstate.ErrFullHealth) {
		t.Fatalf("a heart at full health: %v", err)
	}
	f.clock.at(f.interval(1))
	before := f.view(t)
	if before.Health != f.rules.Health-f.bosses.At(0).AttackDamage || before.Boss.Attacks != 1 {
		t.Fatalf("after the first attack: health %d, attacks %d", before.Health, before.Boss.Attacks)
	}
	v := f.activate(t, 0, heart)
	if v.Health != before.Health+1 || v.Boss.Attacks != 1 || !v.Boss.NextAttackAt.Equal(*before.Boss.NextAttackAt) ||
		!v.Boss.PhaseStartedAt.Equal(before.Boss.PhaseStartedAt) || len(v.Powerups.Held) != 0 {
		t.Fatalf("after the heart: health %d (was %d), boss %+v (was %+v), held %v", v.Health, before.Health, v.Boss, before.Boss, v.Powerups.Held)
	}
	f.restart(t)
	if got := f.view(t); got.Health != v.Health || got.Boss.Attacks != 1 {
		t.Fatalf("after a restart: health %d, attacks %d", got.Health, got.Boss.Attacks)
	}

	big := started(t, fixtureOptions{attacks: true, rules: func(r *teamstate.Rules) { r.PowerupHeart = 5 }})
	big.clock.at(big.interval(1))
	big.grant(t, heart)
	if v := big.activate(t, 0, heart); v.Health != big.rules.Health {
		t.Fatalf("a heart of 5 after one attack: health %d, want game.health %d", v.Health, big.rules.Health)
	}

	ko := started(t, fixtureOptions{attacks: true, bossRules: func(r *bosses.Rules) { r.AttackDamage[1] = 100 }})
	ko.clock.at(ko.interval(1))
	ko.grant(t, heart) // a knocked-out team can still be granted one
	if _, err := ko.state.Activate(ctx, ko.users[0], heart); !errors.Is(err, teamstate.ErrKnockedOut) {
		t.Fatalf("a heart while knocked out: %v", err)
	}
}

// TestShortQRCooldownPowerup checks the short QR cooldown, with
// game.qr_cooldown on: activated at the start, it's active until
// powerups.short_qr_window; a cast in the window cools its waypoints for
// powerups.short_qr_cooldown (marked short_qr_cooldown); activating it again
// while it runs is powerup_active; a restart mid-window keeps it; the window
// ends by itself (powerup_ended) after the cast's cooldown ended
// (qr_cooldown_ended); a cast after the window cools for game.qr_cooldown.
func TestShortQRCooldownPowerup(t *testing.T) {
	f := qrStarted(t, fixtureOptions{})
	ctx := context.Background()
	short, window := f.rules.ShortQRCooldown, f.rules.ShortQRWindow
	if short <= 0 || short >= f.rules.QRCooldown || window <= f.tick(10) {
		t.Skipf("test assumes 0 < powerups.short_qr_cooldown < game.qr_cooldown and a window over 10 s (got %v, %v, %v)", short, f.rules.QRCooldown, window)
	}
	sub := f.hub.Subscribe(f.users[2])
	defer sub.Close()
	kind := teamstate.PowerupShortQRCooldown
	f.grant(t, kind)
	expectType(t, sub, "grant", teamstate.PowerupGranted)
	v := f.activate(t, 0, kind)
	expectType(t, sub, "activate", teamstate.PowerupActivated)
	if got := active(v); !slices.Equal(got, []string{"short_qr_cooldown@" + window.String()}) {
		t.Fatalf("after activation: active %v", got)
	}
	f.grant(t, kind)
	expectType(t, sub, "regrant", teamstate.PowerupGranted)
	if _, err := f.state.Activate(ctx, f.users[0], kind); !errors.Is(err, teamstate.ErrPowerupActive) {
		t.Fatalf("activating while it runs: %v", err)
	}

	res := f.castQuad(t, 0) // cast at 4 s, inside the window
	for range 1 + len(quad) {
		nextState(t, sub)
	}
	if !res.State.LastCast.ShortQRCooldown {
		t.Fatalf("a cast in the window isn't marked short_qr_cooldown: %+v", res.State.LastCast)
	}
	if got := qrIDs(t, res.State, start.Add(f.tick(4)+short)); !slices.Equal(got, quad) {
		t.Fatalf("the quadrilateral's qr_cooldowns %v", got)
	}
	f.restart(t)
	if got := active(f.view(t)); !slices.Equal(got, []string{"short_qr_cooldown@" + window.String()}) {
		t.Fatalf("after a restart: active %v", got)
	}

	f.clock.at(f.tick(4) + short)
	expectType(t, sub, "the quadrilateral's cooldown ends", teamstate.QRCooldownEnded)
	f.clock.at(window)
	if v := expectType(t, sub, "the window ends", teamstate.PowerupEnded); len(v.Powerups.Active) != 0 {
		t.Fatalf("after the window: active %v", active(v))
	}
	expectNoEvent(t, sub, "nothing else due")

	t0 := window + f.tick(10)
	f.selectAt(t, t0, 0, "triangle")
	var last teamstate.ScanResult
	for i, w := range triangle {
		last = f.scanAt(t, t0+f.tick(float64(i+1)), i, w)
	}
	castAt := start.Add(t0 + f.tick(3))
	if last.State.LastCast.ShortQRCooldown {
		t.Fatal("a cast after the window is marked short_qr_cooldown")
	}
	if got := qrIDs(t, last.State, castAt.Add(f.rules.QRCooldown)); !slices.Equal(got, triangle) {
		t.Fatalf("the triangle's qr_cooldowns %v", got)
	}
}

// TestShortQRWindowEndsWithAttack sets powerups.short_qr_window to boss 1's
// attack interval, so a window activated at the start ends at the very
// moment of the first attack: one event, boss_attacked, carrying both.
func TestShortQRWindowEndsWithAttack(t *testing.T) {
	f := started(t, fixtureOptions{attacks: true})
	f.rules.ShortQRWindow = f.interval(1) // known only once the bosses load
	f.restart(t)
	sub := f.hub.Subscribe(f.users[0])
	defer sub.Close()
	f.grant(t, teamstate.PowerupShortQRCooldown)
	f.activate(t, 0, teamstate.PowerupShortQRCooldown)
	nextState(t, sub)
	nextState(t, sub)
	f.clock.at(f.interval(1))
	if v := expectType(t, sub, "attack and window end", teamstate.BossAttacked); v.Boss.Attacks != 1 || len(v.Powerups.Active) != 0 {
		t.Fatalf("attack and window end: attacks %d, active %v", v.Boss.Attacks, active(v))
	}
	expectNoEvent(t, sub, "one event for both")
}

// TestInstantCast checks the instant cast, with game.qr_cooldown on: right
// after the quadrilateral (its class on cooldown, its waypoints cooling), with the
// triangle selected and 2003 scanned, the leader instant-casts the quadrilateral
// again: allowed, last_cast instant and boosted by an armed multiplier, cast
// by the leader, already_completed, the selection and stack purged, visited
// waypoints unchanged, the quadrilateral's waypoints cooling from the new cast.
// Refused: unknown waypoints, waypoints making no pattern, a member, the
// powerup not held.
func TestInstantCast(t *testing.T) {
	f := qrStarted(t, fixtureOptions{})
	ctx := context.Background()
	if _, err := f.state.InstantCast(ctx, f.users[0], []string{"9999"}); !errors.Is(err, teamstate.ErrUnknownWaypoint) {
		t.Fatalf("an unknown waypoint: %v", err)
	}
	if _, err := f.state.InstantCast(ctx, f.users[0], []string{"2001"}); !errors.Is(err, teamstate.ErrUnknownPattern) {
		t.Fatalf("waypoints making no pattern: %v", err)
	}
	if _, err := f.state.InstantCast(ctx, f.users[0], quad); !errors.Is(err, teamstate.ErrPowerupNotHeld) {
		t.Fatalf("not held: %v", err)
	}
	f.castQuad(t, 0)
	f.selectAt(t, f.tick(10), 0, "triangle")
	f.scanAt(t, f.tick(11), 3, "2003")
	before := f.view(t)

	f.grant(t, teamstate.PowerupInstantCast)
	f.grant(t, teamstate.PowerupDamageMultiplier)
	f.activate(t, 0, teamstate.PowerupDamageMultiplier)
	if _, err := f.state.InstantCast(ctx, f.users[1], quad); !errors.Is(err, teamstate.ErrNotLeader) {
		t.Fatalf("a member: %v", err)
	}
	f.clock.at(f.tick(20))
	v, err := f.state.InstantCast(ctx, f.users[0], []string{"2008", "2007", "2002", "2001"})
	if err != nil {
		t.Fatal(err)
	}
	lc := v.LastCast
	want := int(math.Round(float64(f.damageOf(t, quad)) * f.rules.PowerupMultiplier))
	if !lc.Instant || !lc.Boosted || lc.Dealt != want || lc.CastBy != f.users[0] || !lc.AlreadyCompleted || lc.Shape != "quadrilateral" ||
		v.CastCount != 2 || v.Selection != nil || len(v.Stack) != 0 || !slices.Equal(v.Waypoints, before.Waypoints) ||
		len(v.Powerups.Held) != 0 || len(v.Powerups.Active) != 0 {
		t.Fatalf("the instant cast: last_cast %+v, casts %d, selection %v, stack %v, waypoints %v (were %v), powerups %+v",
			lc, v.CastCount, v.Selection, v.Stack, v.Waypoints, before.Waypoints, v.Powerups)
	}
	if got := qrIDs(t, v, start.Add(f.tick(20)+f.rules.QRCooldown)); !slices.Equal(got, quad) {
		t.Fatalf("qr_cooldowns after the instant cast: %v", got)
	}
	if _, err := f.state.InstantCast(ctx, f.users[0], quad); !errors.Is(err, teamstate.ErrPowerupNotHeld) {
		t.Fatalf("a resent instant cast: %v", err)
	}
	var instant bool
	if err := f.db.Pool().QueryRow(ctx, "SELECT instant FROM team_cast WHERE team_id = $1 ORDER BY id DESC LIMIT 1", f.teamID).Scan(&instant); err != nil || !instant {
		t.Fatalf("team_cast.instant %v (%v)", instant, err)
	}
	cs := pgtest.Must(f.state.CastsForUser(f.users[0]))
	f.restart(t)
	if after := pgtest.Must(f.state.CastsForUser(f.users[0])); string(pgtest.Must(json.Marshal(after))) != string(pgtest.Must(json.Marshal(cs))) {
		t.Fatalf("the cast log across a restart\nbefore: %+v\nafter:  %+v", cs, after)
	}
}

// TestInstantCastLockedPattern checks that an instant cast doesn't bypass
// the unlock: a 5-point pentagon before the team has defeated
// game.unlock_after_bosses bosses is pattern_locked.
func TestInstantCastLockedPattern(t *testing.T) {
	pent := []string{"2003", "2004", "2005", "2006", "2009"}
	f := started(t)
	if f.rules.LockedPatternPoints != 5 || f.rules.UnlockAfterBosses < 1 {
		t.Skipf("test assumes locked_pattern_points = 5 and unlock_after_bosses >= 1 (got %d, %d)", f.rules.LockedPatternPoints, f.rules.UnlockAfterBosses)
	}
	f.grant(t, teamstate.PowerupInstantCast)
	if _, err := f.state.InstantCast(context.Background(), f.users[0], pent); !errors.Is(err, teamstate.ErrPatternLocked) {
		t.Fatalf("a locked pentagon: %v", err)
	}
	if v := f.view(t); !slices.Equal(v.Powerups.Held, []string{"instant_cast"}) || v.CastCount != 0 {
		t.Fatalf("a refused instant cast changed the state: held %v, casts %d", v.Powerups.Held, v.CastCount)
	}
}

// TestPowerupsAcrossRestart checks that a restart reproduces powerups
// exactly: held kinds, an armed multiplier and a running short QR window
// (JSON equal, version aside), and that a window which ended while the
// backend was down is gone.
func TestPowerupsAcrossRestart(t *testing.T) {
	f := qrStarted(t, fixtureOptions{})
	for _, k := range []teamstate.Powerup{teamstate.PowerupHeart, teamstate.PowerupInstantCast,
		teamstate.PowerupDamageMultiplier, teamstate.PowerupShortQRCooldown} {
		f.grant(t, k)
	}
	f.activate(t, 0, teamstate.PowerupDamageMultiplier)
	f.activate(t, 0, teamstate.PowerupShortQRCooldown)
	f.clock.at(f.tick(30))
	before := pgtest.Must(json.Marshal(f.view(t).Powerups))
	f.restart(t)
	if after := pgtest.Must(json.Marshal(f.view(t).Powerups)); string(after) != string(before) {
		t.Fatalf("powerups across a restart\nbefore: %s\nafter:  %s", before, after)
	}

	f.state.Close() // down while the window ends
	f.clock.at(f.rules.ShortQRWindow + f.tick(1))
	f.restart(t)
	if got := active(f.view(t)); !slices.Equal(got, []string{"damage_multiplier"}) {
		t.Fatalf("restarted after the window: active %v", got)
	}
}

// TestPowerupReconcile checks a failed activation: with team_powerup.held
// cleared by hand, activating the multiplier fails to persist, memory
// reloads from Postgres (held empty, nothing armed) and is published as
// resync.
func TestPowerupReconcile(t *testing.T) {
	f := started(t)
	sub := f.hub.Subscribe(f.users[0])
	defer sub.Close()
	f.grant(t, teamstate.PowerupDamageMultiplier)
	nextState(t, sub)
	if _, err := f.db.Pool().Exec(context.Background(), "UPDATE team_powerup SET held = false WHERE team_id = $1", f.teamID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.state.Activate(context.Background(), f.users[0], teamstate.PowerupDamageMultiplier); err == nil {
		t.Fatal("activating with the row cleared should fail")
	}
	if v := expectType(t, sub, "reconcile", live.Resync); len(v.Powerups.Held) != 0 || len(v.Powerups.Active) != 0 {
		t.Fatalf("after the reconcile: %+v", v.Powerups)
	}
}

// TestConcurrentPowerups checks races: 8 simultaneous grants of one kind
// grant exactly one; 8 simultaneous activations of it use exactly one, the
// rest refused (powerup_not_held).
func TestConcurrentPowerups(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	const n = 8
	var wg sync.WaitGroup
	granted := make([]bool, n)
	for i := range n {
		wg.Go(func() {
			res, err := f.state.GrantPowerup(ctx, f.teamID, teamstate.PowerupDamageMultiplier)
			if err != nil {
				t.Error(err)
			}
			granted[i] = res.Granted
		})
	}
	wg.Wait()
	if got := countTrue(granted); got != 1 {
		t.Fatalf("%d of %d concurrent grants granted, want 1", got, n)
	}
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() { _, errs[i] = f.state.Activate(ctx, f.users[0], teamstate.PowerupDamageMultiplier) })
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, teamstate.ErrPowerupNotHeld):
			t.Fatalf("a losing activation: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d of %d concurrent activations succeeded, want 1", ok, n)
	}
}

// countTrue counts the true values in bs.
func countTrue(bs []bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// TestLoadRefusesBadPowerups checks that Load refuses team_powerup rows it
// can't rebuild: a damage_multiplier activated past the end of the cast log
// (it would arm a cast that never happened), and a kind this code doesn't
// have (the CHECK dropped here to plant one: code and schema out of step).
func TestLoadRefusesBadPowerups(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	load := func() error {
		_, err := teamstate.Load(ctx, pgtest.Open(t, f.dbURL), f.atlas, f.bosses, f.teams, passQR{}, f.qtes, f.hub, pgtest.Quiet, f.rules)
		return err
	}
	if _, err := f.db.Pool().Exec(ctx, `
		INSERT INTO team_powerup (team_id, kind, activated_at, activated_casts) VALUES ($1, 'damage_multiplier', $2, 3)`, f.teamID, start); err != nil {
		t.Fatal(err)
	}
	if err := load(); err == nil || !strings.Contains(err.Error(), "damage_multiplier activated at 3 casts, but the log has 0") {
		t.Fatalf("load with an activation past the log: %v", err)
	}
	if _, err := f.db.Pool().Exec(ctx, "DELETE FROM team_powerup; ALTER TABLE team_powerup DROP CONSTRAINT team_powerup_kind_check"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Pool().Exec(ctx, "INSERT INTO team_powerup (team_id, kind, held) VALUES ($1, 'shield', true)", f.teamID); err != nil {
		t.Fatal(err)
	}
	if err := load(); err == nil || !strings.Contains(err.Error(), `unknown kind "shield"`) {
		t.Fatalf("load with a shield: %v", err)
	}
}
