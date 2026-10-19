package tests

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/teamstate"
)

// squareDamage is the seed square's spell damage, from the atlas.
func (f *fixtureEnv) squareDamage(t *testing.T) int {
	t.Helper()
	set := pgtest.Must(f.atlas.SetOf(square))
	p, ok := f.atlas.PatternFor(set)
	if !ok {
		t.Fatal("the seed square isn't a pattern")
	}
	return f.atlas.Pattern(p).Spell.Damage
}

// castSquare selects the square and scans it, starting at round × 100 s, and
// returns the completing scan's result.
func (f *fixtureEnv) castSquare(t *testing.T, round int) teamstate.ScanResult {
	t.Helper()
	t0 := time.Duration(round) * f.tick(100)
	f.selectAt(t, t0, 0, "square")
	var res teamstate.ScanResult
	for i, w := range square {
		res = f.scanAt(t, t0+f.tick(float64(i+1)), i, w)
	}
	expectOutcome(t, "cast", res, teamstate.OutcomePatternCompleted)
	return res
}

// restart loads a new store from the same database, as a backend restart
// would, and makes it the fixture's store.
func (f *fixtureEnv) restart(t *testing.T) {
	t.Helper()
	f.state = pgtest.Must(teamstate.Load(context.Background(), pgtest.Open(t, f.dbURL), f.atlas, f.bosses, f.teams, f.hub, pgtest.Quiet, f.rules))
}

// TestCastsDamageBosses casts the square until two bosses fall, checking after
// every cast, against a model built from the seed's HPs: the damage dealt
// (the pattern's, multiplier 1), the boss hit, the progress, a defeat moving
// to the next boss with the excess lost, the event type (spell_cast, or
// boss_defeated), team_boss in Postgres, and a restart reproducing it all.
func TestCastsDamageBosses(t *testing.T) {
	f := started(t)
	dmg := f.squareDamage(t)
	sub := f.hub.Subscribe(f.users[2])
	defer sub.Close()

	if v := f.view(t); v.BossesDefeated != 0 || v.Boss == nil || v.Boss.Serial != 1 || v.Boss.Damage != 0 || v.DamageMultiplier != 1 {
		t.Fatalf("before any cast: boss %+v, defeated %d, multiplier %v", v.Boss, v.BossesDefeated, v.DamageMultiplier)
	}
	defeated, progress := 0, 0
	for round := 0; defeated < 2; round++ {
		res := f.castSquare(t, round)
		hit := defeated + 1
		progress += dmg
		wantDefeat := progress >= f.bosses.At(defeated).HP
		if wantDefeat {
			defeated, progress = defeated+1, 0
		}
		st, lc := res.State, res.State.LastCast
		if lc.Dealt != dmg || lc.Boss == nil || *lc.Boss != hit || lc.DefeatedBoss != wantDefeat {
			t.Fatalf("round %d: cast %+v, want %d dealt to boss %d, defeated %v", round, lc, dmg, hit, wantDefeat)
		}
		if st.BossesDefeated != defeated || st.Boss == nil || st.Boss.Serial != defeated+1 || st.Boss.Damage != progress {
			t.Fatalf("round %d: boss %+v, defeated %d; want boss %d at %d, %d defeated", round, st.Boss, st.BossesDefeated, defeated+1, progress, defeated)
		}
		wantType := teamstate.SpellCast
		if wantDefeat {
			wantType = teamstate.BossDefeated
		}
		var typ live.EventType
		for range 5 { // the selection and four scans
			typ, _ = nextState(t, sub)
		}
		if typ != wantType {
			t.Fatalf("round %d: event %q, want %q", round, typ, wantType)
		}
	}

	var rowDefeated, rowDamage int
	var rowMultiplier float64
	if err := f.db.Pool().QueryRow(context.Background(), "SELECT defeated, damage, multiplier FROM team_boss WHERE team_id = $1",
		f.teamID).Scan(&rowDefeated, &rowDamage, &rowMultiplier); err != nil || rowDefeated != defeated || rowDamage != progress || rowMultiplier != 1 {
		t.Fatalf("team_boss: %d, %d, %v (%v)", rowDefeated, rowDamage, rowMultiplier, err)
	}
	// The version starts again after a restart; compare everything else.
	withoutVersion := func(v teamstate.View) string {
		var m map[string]any
		pgtest.Must(0, json.Unmarshal(pgtest.Must(json.Marshal(v)), &m))
		delete(m, "version")
		return string(pgtest.Must(json.Marshal(m)))
	}
	before := withoutVersion(f.view(t))
	casts := pgtest.Must(json.Marshal(pgtest.Must(f.state.CastsForUser(f.users[0]))))
	f.restart(t)
	after := withoutVersion(f.view(t))
	castsAfter := pgtest.Must(json.Marshal(pgtest.Must(f.state.CastsForUser(f.users[0]))))
	if before != after || string(casts) != string(castsAfter) {
		t.Fatalf("across restart\nbefore: %s\nafter:  %s\ncasts before: %s\ncasts after:  %s", before, after, casts, castsAfter)
	}
}

// TestDamageMultiplier sets a team's multiplier to 1.5 in Postgres, restarts,
// and casts: the cast deals the pattern's damage × 1.5, rounded, while
// damage still reports the pattern's; the multiplier survives the cast's
// write (team_boss keeps it) and another restart.
func TestDamageMultiplier(t *testing.T) {
	f := started(t)
	if _, err := f.db.Pool().Exec(context.Background(),
		"INSERT INTO team_boss (team_id, multiplier) VALUES ($1, 1.5)", f.teamID); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	if m := f.view(t).DamageMultiplier; m != 1.5 {
		t.Fatalf("multiplier after restart: %v", m)
	}
	dmg := f.squareDamage(t)
	want := int(math.Round(float64(dmg) * 1.5))
	lc := f.castSquare(t, 0).State.LastCast
	if lc.Damage != dmg || lc.Dealt != want {
		t.Fatalf("cast: damage %d dealt %d, want %d and %d", lc.Damage, lc.Dealt, dmg, want)
	}
	f.restart(t)
	if v := f.view(t); v.DamageMultiplier != 1.5 || v.LastCast.Dealt != want {
		t.Fatalf("after the cast and a restart: multiplier %v, dealt %d", v.DamageMultiplier, v.LastCast.Dealt)
	}
}

// TestEveryBossDefeated gives every boss 1 HP, so each cast defeats one: after
// the last, boss is null and bosses_defeated is the count; casting on still
// works (the pattern and the cast log count it) but deals nothing and hits
// no boss.
func TestEveryBossDefeated(t *testing.T) {
	f := started(t, fixtureOptions{sql: "UPDATE boss SET hp = 1"})
	n := f.bosses.Count()
	for round := range n {
		if lc := f.castSquare(t, round).State.LastCast; !lc.DefeatedBoss || *lc.Boss != round+1 {
			t.Fatalf("round %d: %+v", round, lc)
		}
	}
	st := f.view(t)
	if st.Boss != nil || st.BossesDefeated != n {
		t.Fatalf("after the last boss: %+v, %d defeated", st.Boss, st.BossesDefeated)
	}
	res := f.castSquare(t, n)
	if lc := res.State.LastCast; lc.Dealt != 0 || lc.Boss != nil || lc.DefeatedBoss || res.State.CastCount != n+1 || res.State.BossesDefeated != n {
		t.Fatalf("a cast after every boss: %+v", res.State)
	}
}

// TestLoadRefusesTooManyDefeated checks that a team_boss row with more bosses
// defeated than game.bosses (it was lowered mid-event) stops the load.
func TestLoadRefusesTooManyDefeated(t *testing.T) {
	f := started(t)
	if _, err := f.db.Pool().Exec(context.Background(),
		"INSERT INTO team_boss (team_id, defeated) VALUES ($1, $2)", f.teamID, f.bosses.Count()+1); err != nil {
		t.Fatal(err)
	}
	_, err := teamstate.Load(context.Background(), pgtest.Open(t, f.dbURL), f.atlas, f.bosses, f.teams, f.hub, pgtest.Quiet, f.rules)
	if err == nil || !strings.Contains(err.Error(), "bosses defeated") {
		t.Fatalf("load: %v", err)
	}
}
