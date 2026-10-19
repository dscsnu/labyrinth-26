// Package postgres owns the database connection, schema migrations, the
// single-instance lock and dev seed data. The SQL for each use case lives in
// that use case's db.go.
package postgres

import (
	"context"
	"embed"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned by use-case db.go functions when a row is missing.
var ErrNotFound = errors.New("not found")

// DB is the shared Postgres connection pool.
type DB struct {
	pool *pgxpool.Pool
}

// Connect opens a pool to url and checks that the database is reachable.
func Connect(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{pool: pool}, nil
}

// Pool is the connection pool, for use-case db.go files.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Ping checks that the database is reachable.
func (db *DB) Ping(ctx context.Context) error { return db.pool.Ping(ctx) }

// Close closes the pool.
func (db *DB) Close() { db.pool.Close() }

// seedFiles holds the dev seed SQL, embedded in the binary.
//
//go:embed seeds/*.sql
var seedFiles embed.FS

// SeedDev2025 loads the 2025 waypoints and patterns as placeholder content
// for local development, followed by the precomputed spell damage values.
// Idempotent. Content is read at startup, so run it while the backend is stopped.
func (db *DB) SeedDev2025(ctx context.Context) error {
	sql, err := seedFiles.ReadFile("seeds/dev_2025.sql")
	if err != nil {
		return err
	}
	if _, err = db.pool.Exec(ctx, string(sql)); err != nil {
		return err
	}
	spellsSQL, err := seedFiles.ReadFile("seeds/dev_2025_spells.sql")
	if err != nil {
		return err
	}
	_, err = db.pool.Exec(ctx, string(spellsSQL))
	return err
}
