package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// migrationFiles holds the migrations, embedded in the binary.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockID is the advisory lock key that stops concurrent instances
// migrating at the same time.
const migrationLockID = 727_2026

// ErrMigrationEdited means a migration file changed after it was applied: its
// SHA-256 no longer matches the one recorded in schema_migrations.
var ErrMigrationEdited = errors.New("migration was edited after it was applied; add a new migration instead")

// Migrate applies every migrations/NNNN_name.sql file not yet recorded in
// schema_migrations, in filename order, each in its own transaction, and
// records its SHA-256. An applied file whose hash changed fails with
// ErrMigrationEdited before anything is applied. Rows from before checksums
// existed (checksum NULL) get the current file's hash recorded.
// Applied files must never be edited; add a new file instead.
func (db *DB) Migrate(ctx context.Context, log *slog.Logger) error {
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockID)

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum TEXT`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[string]*string{} // version → recorded checksum (nil before checksums)
	rows, err := conn.Query(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return err
	}
	var version string
	var checksum *string
	if _, err := pgx.ForEachRow(rows, []any{&version, &checksum}, func() error {
		applied[version] = checksum
		return nil
	}); err != nil {
		return err
	}

	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)

	type file struct{ version, sql, sum string }
	var pending []file
	for _, name := range names {
		sql, err := migrationFiles.ReadFile(name)
		if err != nil {
			return err
		}
		f := file{
			version: strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql"),
			sql:     string(sql),
			sum:     fmt.Sprintf("%x", sha256.Sum256(sql)),
		}
		recorded, ok := applied[f.version]
		switch {
		case !ok:
			pending = append(pending, f)
		case recorded == nil:
			if _, err := conn.Exec(ctx, "UPDATE schema_migrations SET checksum = $2 WHERE version = $1", f.version, f.sum); err != nil {
				return fmt.Errorf("record checksum of %s: %w", f.version, err)
			}
		case *recorded != f.sum:
			return fmt.Errorf("migration %s: %w", f.version, ErrMigrationEdited)
		}
	}

	for _, f := range pending {
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, f.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)", f.version, f.sum)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", f.version, err)
		}
		log.Info("applied migration", "version", f.version)
	}

	return nil
}
