package tests

import (
	"context"
	"errors"
	"slices"
	"testing"

	"labyrinth/internal/bosses"
	"labyrinth/internal/teamstate"
)

// mixedShape adds a waypoint, 2097, and a shape mixing a 3-point (class_3)
// and a 4-point (class_4) pattern through it, neither inside the other, so a
// cooldown on either class leaves the shape selectable but narrows what
// fits. (Every 2026 shape is one size, and every set of the 10 checkpoints
// is already a pattern, so both go through a test-only waypoint.)
const mixedShape = `
	INSERT INTO waypoint (id, name, latitude, longitude) VALUES ('2097', 'Mixed', 0, 0);
	INSERT INTO shape (id, name) VALUES ('mixed', 'Mixed');
	INSERT INTO pattern (id, shape_id, damage, spell_class_id) VALUES (1006, 'mixed', 50, 'class_3'), (1007, 'mixed', 80, 'class_4');
	INSERT INTO pattern_waypoint (pattern_id, waypoint_id) VALUES
		(1006, '2005'), (1006, '2006'), (1006, '2097'),
		(1007, '2001'), (1007, '2002'), (1007, '2008'), (1007, '2097');`

// TestSpellCooldown checks the spell cooldown: the class of the team's last
// cast can't be cast again until a spell of another class is.
//
//   - before any cast: no cooldown, nothing on cooldown
//   - after the quadrilateral (class_4): cooldown_class class_4; the shape
//     made only of class_4 patterns (quadrilateral) is in cooldown_shapes and
//     selecting one is shape_on_cooldown (nothing written or published); the
//     mixed shape is selectable, but a scan that only fits its class_4
//     pattern is wrong
//   - after the mixed shape's class_3 pattern: cooldown_class class_3; the
//     quadrilateral is selectable and casts again; triangle is on cooldown
//   - a restart rebuilds the cooldown from the cast log
//   - a defeated boss doesn't clear it
func TestSpellCooldown(t *testing.T) {
	f := started(t, fixtureOptions{sql: mixedShape})
	ctx := context.Background()
	sub := f.hub.Subscribe(f.users[0])
	defer sub.Close()

	if v := f.view(t); v.CooldownClass != nil || len(v.CooldownShapes) != 0 {
		t.Fatalf("before any cast: cooldown %v, shapes %v", v.CooldownClass, v.CooldownShapes)
	}

	f.castQuad(t, 0) // also defeats boss 1 (25 HP)
	for range 1 + len(quad) {
		nextState(t, sub)
	}
	v := f.view(t)
	if v.CooldownClass == nil || *v.CooldownClass != "class_4" || v.BossesDefeated != 1 ||
		!slices.Equal(v.CooldownShapes, []string{"quadrilateral"}) {
		t.Fatalf("after the quadrilateral: cooldown %v, shapes %v, defeated %d", v.CooldownClass, v.CooldownShapes, v.BossesDefeated)
	}
	seq := v.Version.Seq
	if _, err := f.state.Select(ctx, f.users[0], "quadrilateral"); !errors.Is(err, teamstate.ErrShapeOnCooldown) {
		t.Fatalf("selecting the quadrilateral on cooldown: %v, want shape_on_cooldown", err)
	}
	expectNoEvent(t, sub, "refused selections")
	if v := f.view(t); v.Version.Seq != seq || v.Selection != nil {
		t.Fatalf("a refused selection changed the state: seq %d (was %d), selection %+v", v.Version.Seq, seq, v.Selection)
	}

	// The mixed shape is selectable: its class_3 pattern isn't on cooldown.
	// 2001 is only in its class_4 pattern, so it's wrong now.
	f.selectAt(t, f.tick(200), 0, "mixed")
	expectOutcome(t, "a point only in the cooled-down pattern", f.scanAt(t, f.tick(201), 0, "2001"), teamstate.OutcomeWrongScan)
	f.selectAt(t, f.tick(202), 0, "mixed")
	var res teamstate.ScanResult
	for i, w := range []string{"2005", "2006", "2097"} {
		res = f.scanAt(t, f.tick(float64(203+i)), i, w)
	}
	expectOutcome(t, "the mixed shape's class_3 pattern", res, teamstate.OutcomePatternCompleted)
	if c := res.State.LastCast.Class; c != "class_3" {
		t.Fatalf("cast class %q, want class_3", c)
	}
	v = f.view(t)
	if v.CooldownClass == nil || *v.CooldownClass != "class_3" || !slices.Equal(v.CooldownShapes, []string{"triangle"}) {
		t.Fatalf("after a class_3 cast: cooldown %v, shapes %v", v.CooldownClass, v.CooldownShapes)
	}

	// A restart rebuilds the cooldown from the cast log.
	f.restart(t)
	v = f.view(t)
	if v.CooldownClass == nil || *v.CooldownClass != "class_3" || !slices.Equal(v.CooldownShapes, []string{"triangle"}) {
		t.Fatalf("after a restart: cooldown %v, shapes %v", v.CooldownClass, v.CooldownShapes)
	}
	if _, err := f.state.Select(ctx, f.users[0], "triangle"); !errors.Is(err, teamstate.ErrShapeOnCooldown) {
		t.Fatalf("selecting the triangle after a restart: %v, want shape_on_cooldown", err)
	}

	// The quadrilateral casts again: class_4 is no longer the last class.
	if res := f.castQuad(t, 3); res.State.LastCast.Class != "class_4" || !res.AlreadyCompleted {
		t.Fatalf("the quadrilateral after a class_3 cast: %+v", res.State.LastCast)
	}
	if v := f.view(t); v.CooldownClass == nil || *v.CooldownClass != "class_4" {
		t.Fatalf("after the second quadrilateral: cooldown %v", v.CooldownClass)
	}
}

// TestCooldownFailedCastKeepsIt checks that a completing scan that fails to
// persist changes nothing: with the database closed, the triangle that would
// clear the quadrilateral's cooldown fails, and the cooldown, the last cast and the
// cast count stay the quadrilateral's.
func TestCooldownFailedCastKeepsIt(t *testing.T) {
	f := started(t)
	f.castQuad(t, 0)
	f.selectAt(t, f.tick(200), 0, "triangle")
	f.scanAt(t, f.tick(201), 0, triangle[0])
	f.scanAt(t, f.tick(202), 1, triangle[1])
	f.db.Close()
	if err := f.scanErrAt(f.tick(203), 2, triangle[2]); err == nil {
		t.Fatal("the completing scan with the database closed should fail")
	}
	v := f.view(t)
	if v.CooldownClass == nil || *v.CooldownClass != "class_4" || v.CastCount != 1 || v.LastCast.Shape != "quadrilateral" {
		t.Fatalf("after a failed cast: cooldown %v, casts %d, last %s", v.CooldownClass, v.CastCount, v.LastCast.Shape)
	}
}

// TestCooldownKnockedOutFirst checks the order of Select's checks: a knocked
// out team selecting a shape on cooldown is told knocked_out, not
// shape_on_cooldown (every boss hits for 100, so boss 2's first attack after
// the quadrilateral defeats boss 1 knocks the team out). ClearSelection still works.
func TestCooldownKnockedOutFirst(t *testing.T) {
	f := started(t, fixtureOptions{attacks: true, bossRules: func(r *bosses.Rules) {
		for serial := range r.AttackDamage {
			r.AttackDamage[serial] = 100
		}
	}})
	res := f.castQuad(t, 0)
	if !res.State.LastCast.DefeatedBoss {
		t.Fatalf("the quadrilateral should defeat boss 1: %+v", res.State.LastCast)
	}
	defeatedAt := res.State.LastCast.CastAt.Sub(start)
	f.clock.at(defeatedAt + f.interval(2))
	if v := f.view(t); !v.KnockedOut || v.CooldownClass == nil || *v.CooldownClass != "class_4" {
		t.Fatalf("knocked out with the quadrilateral on cooldown: knocked %v, cooldown %v", v.KnockedOut, v.CooldownClass)
	}
	if _, err := f.state.Select(context.Background(), f.users[0], "quadrilateral"); !errors.Is(err, teamstate.ErrKnockedOut) {
		t.Fatalf("selecting a shape on cooldown while knocked out: %v, want knocked_out", err)
	}
	if _, err := f.state.Select(context.Background(), f.users[0], "triangle"); !errors.Is(err, teamstate.ErrKnockedOut) {
		t.Fatalf("selecting a castable shape while knocked out: %v, want knocked_out", err)
	}
}

// TestCooldownOnBigPatterns checks the cooldown once pentagons are unlocked
// under config.toml's schedule (every boss at 1 HP, defeated by triangles
// and lines until then; a seed pentagon): casting the
// pentagon puts class_5 on cooldown, the pentagon moves to cooldown_shapes
// (not locked_shapes) and selecting it is shape_on_cooldown; a 4-point cast
// opens it again.
func TestCooldownOnBigPatterns(t *testing.T) {
	pent := []string{"2003", "2004", "2005", "2006", "2009"}
	f := started(t, fixtureOptions{unlocks: true, sql: "UPDATE boss SET hp = 1"})
	need, ok := bossesFor(f.rules, 5)
	if !ok || need < 1 {
		t.Skipf("test assumes the unlock schedule locks pentagons at the start (game.unlocks %v)", f.rules.Unlocks)
	}
	for round := range need {
		f.castSmall(t, round) // each defeats a 1-HP boss
	}
	if v := f.view(t); v.UnlockedPoints < 5 || slices.Contains(v.LockedShapes, "pentagon") {
		t.Fatalf("after the unlock: unlocked %d, locked %v", v.UnlockedPoints, v.LockedShapes)
	}
	f.cast(t, 50, "pentagon", pent)
	v := f.view(t)
	if v.CooldownClass == nil || *v.CooldownClass != "class_5" || !slices.Equal(v.CooldownShapes, []string{"pentagon"}) || slices.Contains(v.LockedShapes, "pentagon") {
		t.Fatalf("after the pentagon: cooldown %v, cooldown shapes %v, locked %v", v.CooldownClass, v.CooldownShapes, v.LockedShapes)
	}
	if _, err := f.state.Select(context.Background(), f.users[0], "pentagon"); !errors.Is(err, teamstate.ErrShapeOnCooldown) {
		t.Fatalf("selecting the pentagon right after its cast: %v, want shape_on_cooldown", err)
	}
	f.castQuad(t, 51)
	f.cast(t, 52, "pentagon", pent)
}

// TestCooldownAfterEveryBoss checks that casting after every boss is
// defeated (dealing nothing) still sets the cooldown.
func TestCooldownAfterEveryBoss(t *testing.T) {
	f := started(t, fixtureOptions{sql: "UPDATE boss SET hp = 1"})
	n := f.bosses.Count()
	for round := range n {
		f.castAlternating(t, round)
	}
	res, _ := f.castAlternating(t, n)
	want := "class_4"
	if n%2 == 1 {
		want = "class_3"
	}
	if res.State.Boss != nil || res.State.LastCast.Dealt != 0 || res.State.CooldownClass == nil || *res.State.CooldownClass != want {
		t.Fatalf("a cast after every boss: boss %+v, dealt %d, cooldown %v (want %s)", res.State.Boss, res.State.LastCast.Dealt, res.State.CooldownClass, want)
	}
}
