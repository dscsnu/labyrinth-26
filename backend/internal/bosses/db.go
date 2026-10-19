package bosses

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// db reads the boss table (migration 0009).
type db struct {
	pool *pgxpool.Pool
}

// selectBosses returns every boss, in serial order.
func (d db) selectBosses(ctx context.Context) ([]Boss, error) {
	rows, err := d.pool.Query(ctx, "SELECT serial, name, asset, description, hp FROM boss ORDER BY serial")
	if err != nil {
		return nil, err
	}
	var b Boss
	list := []Boss{}
	_, err = pgx.ForEachRow(rows, []any{&b.Serial, &b.Name, &b.Asset, &b.Description, &b.HP}, func() error {
		list = append(list, b)
		return nil
	})
	return list, err
}
