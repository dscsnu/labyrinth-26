package tests

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"labyrinth/internal/bosses"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/teamstate"
)

// interval returns the seed boss with that serial's attack interval.
func (f *fixtureEnv) interval(serial int) time.Duration {
	return f.bosses.At(serial - 1).AttackInterval()
}

// expectType reads the next team_state event and fails unless it's typ.
func expectType(t *testing.T, sub *live.Subscription, what string, typ live.EventType) teamstate.View {
	t.Helper()
	got, v := nextState(t, sub)
	if got != typ {
		t.Fatalf("%s: event %q, want %q", what, got, typ)
	}
	return v
}

// expectNoEvent fails if sub has a team_state waiting.
func expectNoEvent(t *testing.T, sub *live.Subscription, what string) {
	t.Helper()
	select {
	case ev := <-sub.C:
		t.Fatalf("%s: unexpected %s event %q", what, ev.Name(), ev.Type())
	default:
	}
}

// TestBossAttacks follows boss 1's attack clock from the team's start:
// nothing before the first interval, an attack exactly at it (boss_attacked,
// health down by its damage, the next attack one interval later), a defeat
// restarting the clock for boss 2 at the cast with the health the team had,
// boss 2's attacks down to 0 (knocked_out, then no timer), selecting and
// scanning refused while clearing still works, and a revive restoring full
// health and a fresh clock.
func TestBossAttacks(t *testing.T) {
	f := started(t, fixtureOptions{attacks: true})
	sub := f.hub.Subscribe(f.users[1])
	defer sub.Close()
	full := f.rules.Health
	b1, b2 := f.bosses.At(0), f.bosses.At(1)

	if v := f.view(t); v.Health != full || v.KnockedOut || v.Boss.Attacks != 0 ||
		!v.Boss.PhaseStartedAt.Equal(start) || !v.Boss.NextAttackAt.Equal(start.Add(f.interval(1))) {
		t.Fatalf("at the start: health %d, boss %+v", v.Health, v.Boss)
	}
	if f.clock.pending() != 1 {
		t.Fatalf("%d attack timers pending, want 1", f.clock.pending())
	}
	f.clock.at(f.interval(1) - time.Microsecond)
	expectNoEvent(t, sub, "just before the first attack")
	f.clock.at(f.interval(1))
	v := expectType(t, sub, "first attack", teamstate.BossAttacked)
	if v.Health != full-b1.AttackDamage || v.Boss.Attacks != 1 || !v.Boss.NextAttackAt.Equal(start.Add(2*f.interval(1))) {
		t.Fatalf("after the first attack: health %d, boss %+v", v.Health, v.Boss)
	}

	// Defeat boss 1, casting right after its first attack.
	t0 := f.interval(1)
	for round := 0; f.view(t).BossesDefeated == 0; round++ {
		f.selectAt(t, t0+f.tick(float64(10*round)), 0, "square")
		for i, w := range square {
			f.scanAt(t, t0+f.tick(float64(10*round+i+1)), i, w)
		}
	}
	v = f.view(t)
	defeatedAt := v.LastCast.CastAt
	if v.Boss.Serial != 2 || !v.Boss.PhaseStartedAt.Equal(defeatedAt) || v.Boss.Attacks != 0 ||
		v.Health != full-b1.AttackDamage || !v.Boss.NextAttackAt.Equal(defeatedAt.Add(f.interval(2))) {
		t.Fatalf("after defeating boss 1: health %d, boss %+v", v.Health, v.Boss)
	}
	for len(sub.C) > 0 { // the casts' events
		<-sub.C
	}

	// Boss 2 attacks until health is 0, one interval at a time (a jump past
	// several applies them all at once, as one event).
	attacks := (v.Health + b2.AttackDamage - 1) / b2.AttackDamage
	for k := 1; k < attacks; k++ {
		f.clock.at(defeatedAt.Sub(start) + time.Duration(k)*f.interval(2))
		expectType(t, sub, "boss 2 attack", teamstate.BossAttacked)
	}
	f.clock.at(defeatedAt.Sub(start) + time.Duration(attacks)*f.interval(2))
	v = expectType(t, sub, "the last attack", teamstate.KnockedOut)
	if v.Health != 0 || !v.KnockedOut || v.Boss.NextAttackAt != nil || f.clock.pending() != 0 {
		t.Fatalf("knocked out: health %d, boss %+v, %d timers", v.Health, v.Boss, f.clock.pending())
	}
	f.clock.at(defeatedAt.Sub(start) + time.Duration(attacks+5)*f.interval(2))
	expectNoEvent(t, sub, "no attacks once knocked out")

	now := defeatedAt.Sub(start) + time.Duration(attacks+5)*f.interval(2)
	if _, err := f.state.Select(context.Background(), f.users[0], "square"); !errors.Is(err, teamstate.ErrKnockedOut) {
		t.Fatalf("select while knocked out: %v", err)
	}
	if err := f.scanErrAt(now, 0, "1001"); !errors.Is(err, teamstate.ErrKnockedOut) {
		t.Fatalf("scan while knocked out: %v", err)
	}
	if _, err := f.state.ClearSelection(context.Background(), f.users[0]); err != nil {
		t.Fatalf("clearing while knocked out should work: %v", err)
	}

	v = pgtest.Must(f.state.Revive(context.Background(), f.teamID))
	revivedAt := start.Add(now)
	if v.Health != full || v.KnockedOut || v.Boss.Serial != 2 || v.Boss.Attacks != 0 || !v.Boss.PhaseStartedAt.Equal(revivedAt) ||
		!v.Boss.NextAttackAt.Equal(revivedAt.Add(f.interval(2))) || f.clock.pending() != 1 {
		t.Fatalf("after revive: health %d, boss %+v, %d timers", v.Health, v.Boss, f.clock.pending())
	}
	expectType(t, sub, "revive", teamstate.Revived)
	f.selectAt(t, now+f.tick(1), 0, "square")
}

// TestAttacksAcrossRestart checks that health is derived, not stored: with
// the store stopped (Close, so no timer runs), the clock passes three of
// boss 1's attacks; a restart applies them at load, and its state equals a
// store that stayed up (compared as JSON, version aside). A revive's phase is
// stored and survives the restart too.
func TestAttacksAcrossRestart(t *testing.T) {
	f := started(t, fixtureOptions{attacks: true})
	b1 := f.bosses.At(0)
	f.state.Close()
	f.clock.at(3*f.interval(1) + f.tick(5))
	if f.clock.pending() != 0 {
		t.Fatalf("Close should stop every timer: %d pending", f.clock.pending())
	}
	if v := f.view(t); v.Boss.Attacks != 0 {
		t.Fatalf("a closed store shouldn't attack: %+v", v.Boss)
	}
	f.restart(t)
	v := f.view(t)
	if v.Boss.Attacks != 3 || v.Health != f.rules.Health-3*b1.AttackDamage || !v.Boss.NextAttackAt.Equal(start.Add(4*f.interval(1))) {
		t.Fatalf("after a restart: health %d, boss %+v", v.Health, v.Boss)
	}

	pgtest.Must(f.state.Revive(context.Background(), f.teamID))
	withoutVersion := func() string {
		var m map[string]any
		pgtest.Must(0, json.Unmarshal(pgtest.Must(json.Marshal(f.view(t))), &m))
		delete(m, "version")
		return string(pgtest.Must(json.Marshal(m)))
	}
	f.clock.at(5*f.interval(1) + f.tick(5)) // one attack after the revive
	before := withoutVersion()
	f.restart(t)
	if after := withoutVersion(); before != after {
		t.Fatalf("across restart\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestWriteCatchesUpAttacks checks that a write never misses an attack a
// late timer hasn't applied: with the timers stopped, a scan after boss 1's
// first attack is preceded by a boss_attacked event, and sees the lower
// health; a select after a knockout-worth of attacks is refused.
func TestWriteCatchesUpAttacks(t *testing.T) {
	f := started(t, fixtureOptions{attacks: true, bossRules: func(r *bosses.Rules) { r.AttackDamage[1] = 4 }})
	sub := f.hub.Subscribe(f.users[0])
	defer sub.Close()
	f.selectAt(t, 0, 0, "square")
	expectType(t, sub, "select", teamstate.ShapeSelected)
	f.state.Close() // timers off: only writes catch up

	f.clock.at(f.interval(1))
	expectNoEvent(t, sub, "no timer")
	res := f.scanAt(t, f.interval(1)+f.tick(1), 0, "1001")
	expectType(t, sub, "caught-up attack", teamstate.BossAttacked)
	expectType(t, sub, "the scan", teamstate.ScanAdded)
	if res.State.Health != f.rules.Health-4 || res.State.Boss.Attacks != 1 {
		t.Fatalf("the scan's state: health %d, boss %+v", res.State.Health, res.State.Boss)
	}

	// game.health 10 at 4 a hit: the third attack knocks the team out.
	knockout := (f.rules.Health + 3) / 4
	f.clock.at(time.Duration(knockout) * f.interval(1))
	if _, err := f.state.Select(context.Background(), f.users[0], "square"); !errors.Is(err, teamstate.ErrKnockedOut) {
		t.Fatalf("select after a knockout's worth of attacks: %v", err)
	}
	expectType(t, sub, "caught-up knockout", teamstate.KnockedOut)
}

// TestReviveErrors checks Revive's refusals: an unknown team, and a team
// that hasn't started.
func TestReviveErrors(t *testing.T) {
	f := started(t, fixtureOptions{lobby: true})
	if _, err := f.state.Revive(context.Background(), "000000"); !errors.Is(err, teamstate.ErrTeamNotFound) {
		t.Fatalf("unknown team: %v", err)
	}
	if _, err := f.state.Revive(context.Background(), f.teamID); !errors.Is(err, teamstate.ErrNotInGame) {
		t.Fatalf("lobby team: %v", err)
	}
}
