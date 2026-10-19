package tests

import (
	"context"
	"strings"
	"testing"

	"labyrinth/internal/bosses"
	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/postgres/pgtest"
)

// three returns a valid three-boss sequence.
func three() []bosses.Boss {
	return []bosses.Boss{
		{Serial: 1, Name: "One", Asset: "bosses/1.png", Description: "first", HP: 100},
		{Serial: 2, Name: "Two", Asset: "bosses/2.png", HP: 200},
		{Serial: 3, Name: "Three", Asset: "bosses/3.png", HP: 300},
	}
}

// TestNew checks New's validation: exactly serials 1…Count (or none at all),
// each with a name, an asset and positive HP, and lookups by index.
func TestNew(t *testing.T) {
	b, err := bosses.New(three(), bosses.Rules{Count: 3})
	if err != nil {
		t.Fatal(err)
	}
	if b.Count() != 3 || b.At(0).Name != "One" || b.At(2).HP != 300 || len(b.All()) != 3 || b.All()[1].Serial != 2 {
		t.Fatalf("bosses: %+v", b.All())
	}
	if empty, err := bosses.New(nil, bosses.Rules{Count: 3}); err != nil || empty.Count() != 0 {
		t.Fatalf("an empty table should load with Count 0: %v", err)
	}

	cases := map[string]struct {
		edit func([]bosses.Boss) []bosses.Boss
		want string
	}{
		"too few": {func(l []bosses.Boss) []bosses.Boss { return l[:2] }, "2 bosses stored; game.bosses is 3"},
		"too many": {func(l []bosses.Boss) []bosses.Boss {
			return append(l, bosses.Boss{Serial: 4, Name: "x", Asset: "a", HP: 1})
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
		_, err := bosses.New(c.edit(three()), bosses.Rules{Count: 3})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

// seedBosses is how many bosses the 2025 dev seed has.
const seedBosses = 7

// TestLoad loads an empty table (Count 0), then the 2025 dev seed's bosses
// with config.toml's game.bosses: all of them, in serial order, HP rising.
// With game.bosses one lower, startup is refused, naming both numbers.
func TestLoad(t *testing.T) {
	n := configtest.Settings(t).Game.Bosses
	if n != seedBosses {
		t.Skipf("the 2025 seed has %d bosses; game.bosses is %d", seedBosses, n)
	}
	ctx := context.Background()
	db := pgtest.Open(t, pgtest.URL(t, "bosses_test"))
	if b := pgtest.Must(bosses.Load(ctx, db, pgtest.Quiet, bosses.Rules{Count: 7})); b.Count() != 0 {
		t.Fatalf("before seeding: %d bosses", b.Count())
	}
	if err := db.SeedDev2025(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := bosses.Load(ctx, db, pgtest.Quiet, bosses.Rules{Count: n})
	if err != nil {
		t.Fatal(err)
	}
	for i, boss := range b.All() {
		if boss.Serial != i+1 || boss.Name == "" || boss.Asset == "" || boss.HP <= 0 {
			t.Fatalf("boss %d: %+v", i, boss)
		}
		if i > 0 && boss.HP <= b.At(i-1).HP {
			t.Fatalf("the seed's HP should rise: boss %d has %d after %d", boss.Serial, boss.HP, b.At(i-1).HP)
		}
	}
	if _, err := bosses.Load(ctx, db, pgtest.Quiet, bosses.Rules{Count: n - 1}); err == nil ||
		!strings.Contains(err.Error(), "game.bosses is") {
		t.Fatalf("a stored count over game.bosses should be refused: %v", err)
	}
}
