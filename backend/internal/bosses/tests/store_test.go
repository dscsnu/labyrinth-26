package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"labyrinth/internal/bosses"
	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/postgres/pgtest"
)

// threeRules are valid rules for three(): every boss attacks every 5 min,
// for 1, 0 and 5.
func threeRules() bosses.Rules {
	return bosses.Rules{Count: 3, AttackInterval: 5 * time.Minute, AttackDamage: map[int]int{1: 1, 2: 0, 3: 5}}
}

// three returns a valid three-boss sequence.
func three() []bosses.Boss {
	return []bosses.Boss{
		{Serial: 1, Name: "One", Asset: "bosses/1.png", Description: "first", HP: 100},
		{Serial: 2, Name: "Two", Asset: "bosses/2.png", HP: 200},
		{Serial: 3, Name: "Three", Asset: "bosses/3.png", HP: 300},
	}
}

// TestNew checks New's validation: exactly serials 1…Count (or none at all),
// each with a name, an asset and positive HP; the attack rules (a whole-second
// interval, a non-negative damage for every boss), filled into each Boss;
// and lookups by index.
func TestNew(t *testing.T) {
	b, err := bosses.New(three(), threeRules())
	if err != nil {
		t.Fatal(err)
	}
	if b.Count() != 3 || b.At(0).Name != "One" || b.At(2).HP != 300 || len(b.All()) != 3 || b.All()[1].Serial != 2 ||
		b.At(0).AttackInterval() != 5*time.Minute || b.At(2).AttackDamage != 5 {
		t.Fatalf("bosses: %+v", b.All())
	}
	if empty, err := bosses.New(nil, threeRules()); err != nil || empty.Count() != 0 {
		t.Fatalf("an empty table should load with Count 0: %v", err)
	}

	// Attack rules: from config.toml, applied to every boss.
	for name, c := range map[string]struct {
		edit func(*bosses.Rules)
		want string
	}{
		"zero interval":        {func(r *bosses.Rules) { r.AttackInterval = 0 }, "game.boss_attack.interval must be whole seconds, at least 1s"},
		"part of a second":     {func(r *bosses.Rules) { r.AttackInterval = 1500 * time.Millisecond }, "must be whole seconds"},
		"no damage for a boss": {func(r *bosses.Rules) { delete(r.AttackDamage, 2) }, "boss 2 has no attack damage"},
		"negative damage":      {func(r *bosses.Rules) { r.AttackDamage[3] = -1 }, "boss 3: attack damage must not be negative"},
	} {
		rules := threeRules()
		c.edit(&rules)
		if _, err := bosses.New(three(), rules); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}

	cases := map[string]struct {
		edit func([]bosses.Boss) []bosses.Boss
		want string
	}{
		"too few": {func(l []bosses.Boss) []bosses.Boss { return l[:2] }, "2 bosses stored; game.bosses is 3"},
		"too many": {func(l []bosses.Boss) []bosses.Boss {
			return append(l, bosses.Boss{Serial: 4, Name: "x", Asset: "a", HP: 1, AttackIntervalS: 1})
		}, "4 bosses stored"},
		"gap":              {func(l []bosses.Boss) []bosses.Boss { l[2].Serial = 4; return l }, "boss serial 4 where 3 was expected"},
		"starts at zero":   {func(l []bosses.Boss) []bosses.Boss { l[0].Serial = 0; return l }, "boss serial 0 where 1 was expected"},
		"no name":          {func(l []bosses.Boss) []bosses.Boss { l[1].Name = ""; return l }, "boss 2 has no name"},
		"no asset":         {func(l []bosses.Boss) []bosses.Boss { l[1].Asset = ""; return l }, "boss 2 has no asset"},
		"zero hp":          {func(l []bosses.Boss) []bosses.Boss { l[0].HP = 0; return l }, "boss 1: hp must be positive"},
		"negative hp":      {func(l []bosses.Boss) []bosses.Boss { l[2].HP = -5; return l }, "boss 3: hp must be positive"},
		"duplicate serial": {func(l []bosses.Boss) []bosses.Boss { l[1].Serial = 1; return l }, "boss serial 1 where 2 was expected"},
	}
	for name, c := range cases {
		rules := threeRules()
		_, err := bosses.New(c.edit(three()), rules)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

// seedRules are config.toml's boss attack rules with count bosses.
func seedRules(t *testing.T, count int) bosses.Rules {
	set := configtest.Settings(t)
	return bosses.Rules{Count: count, AttackInterval: set.Game.BossAttack.Interval.Duration, AttackDamage: set.Game.BossAttack.BossDamage()}
}

// seedBosses is how many bosses the 2026 seed has.
const seedBosses = 6

// TestLoad loads an empty table (Count 0), then the 2026 seed's bosses
// with config.toml's game.bosses: all of them, in serial order, HP rising.
// With game.bosses one lower, startup is refused, naming both numbers.
func TestLoad(t *testing.T) {
	n := configtest.Settings(t).Game.Bosses
	if n != seedBosses {
		t.Skipf("the 2026 seed has %d bosses; game.bosses is %d", seedBosses, n)
	}
	ctx := context.Background()
	db := pgtest.Open(t, pgtest.URL(t, "bosses_test"))
	if b := pgtest.Must(bosses.Load(ctx, db, pgtest.Quiet, seedRules(t, 7))); b.Count() != 0 {
		t.Fatalf("before seeding: %d bosses", b.Count())
	}
	if err := db.Seed2026(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := bosses.Load(ctx, db, pgtest.Quiet, seedRules(t, n))
	if err != nil {
		t.Fatal(err)
	}
	for i, boss := range b.All() {
		if boss.Serial != i+1 || boss.Name == "" || boss.Asset == "" || boss.HP <= 0 || boss.AttackIntervalS <= 0 || boss.AttackDamage < 0 {
			t.Fatalf("boss %d: %+v", i, boss)
		}
		if i > 0 && boss.HP <= b.At(i-1).HP {
			t.Fatalf("the seed's HP should rise: boss %d has %d after %d", boss.Serial, boss.HP, b.At(i-1).HP)
		}
	}
	if _, err := bosses.Load(ctx, db, pgtest.Quiet, seedRules(t, n-1)); err == nil ||
		!strings.Contains(err.Error(), "game.bosses is") {
		t.Fatalf("a stored count over game.bosses should be refused: %v", err)
	}
}
