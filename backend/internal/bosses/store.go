package bosses

import (
	"context"
	"fmt"
	"log/slog"

	"labyrinth/internal/platform/postgres"
)

// The in-memory layout. Built once by Load and never changed, so there are no
// locks:
//
//	Bosses
//	  list  []Boss   index → boss, where index = serial − 1
//
// There's no map: serials are dense (exactly 1…game.bosses, checked by New),
// so the serial itself is the key, and a lookup is one slice index, O(1),
// with no hashing. Teams progress through the list in index order.

// Rules are the bosses' settings from config.toml.
type Rules struct {
	// Count is how many bosses every team fights (game.bosses); the content
	// must be exactly serials 1…Count.
	Count int
}

// Load reads the boss table, validates it against rules, and builds the
// Bosses. Call once at startup, before any use case that reads it
// (teamstate).
func Load(ctx context.Context, pg *postgres.DB, log *slog.Logger, rules Rules) (*Bosses, error) {
	list, err := db{pool: pg.Pool()}.selectBosses(ctx)
	if err != nil {
		return nil, fmt.Errorf("load bosses: %w", err)
	}
	b, err := New(list, rules)
	if err != nil {
		return nil, fmt.Errorf("invalid bosses: %w", err)
	}
	if len(b.list) == 0 {
		log.Warn("no bosses stored: casts hit no boss until boss content is loaded and the backend restarted",
			"game.bosses", rules.Count)
	}
	log.Info("loaded bosses", "bosses", len(b.list))
	return b, nil
}

// New validates list (in serial order) against rules and builds the Bosses:
// exactly serials 1…rules.Count, each with a name, an asset and positive HP.
// An empty list is allowed too, like an empty atlas, so a fresh database
// boots before content is loaded: then Count is 0 and no cast hits a boss.
// Load calls it with what Postgres holds; tests call it with a fixture.
func New(list []Boss, rules Rules) (*Bosses, error) {
	if len(list) != 0 && len(list) != rules.Count {
		return nil, fmt.Errorf("%d bosses stored; game.bosses is %d (the boss table must be exactly serials 1-%d)",
			len(list), rules.Count, rules.Count)
	}
	b := &Bosses{list: make([]Boss, len(list))}
	for i, boss := range list {
		switch {
		case boss.Serial != i+1:
			return nil, fmt.Errorf("boss serial %d where %d was expected: serials must be 1-%d with no gaps", boss.Serial, i+1, rules.Count)
		case boss.Name == "":
			return nil, fmt.Errorf("boss %d has no name", boss.Serial)
		case boss.Asset == "":
			return nil, fmt.Errorf("boss %d has no asset", boss.Serial)
		case boss.HP <= 0:
			return nil, fmt.Errorf("boss %d: hp must be positive, got %d", boss.Serial, boss.HP)
		}
		b.list[i] = boss
	}
	return b, nil
}
