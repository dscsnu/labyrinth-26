package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/platform/postgres/pgtest"
)

// TestOnlyOneInstance checks that a second backend can't take the instance
// lock until the first releases it.
func TestOnlyOneInstance(t *testing.T) {
	dbURL := pgtest.URL(t, "postgres_test")
	ctx := context.Background()
	a, b := pgtest.Open(t, dbURL), pgtest.Open(t, dbURL)

	lockA, err := a.AcquireInstanceLock(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.AcquireInstanceLock(ctx, time.Hour); !errors.Is(err, postgres.ErrAnotherInstance) {
		t.Fatalf("second instance: got %v, want ErrAnotherInstance", err)
	}
	lockA.Release()
	lockA.Release() // twice is fine
	lockB, err := b.AcquireInstanceLock(ctx, time.Hour)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	lockB.Release()
}

// TestLostInstanceLockIsNoticed checks that killing the lock's connection
// (as a Postgres restart would) closes Lost within a few check intervals,
// after which another instance can take the lock.
func TestLostInstanceLockIsNoticed(t *testing.T) {
	dbURL := pgtest.URL(t, "postgres_test")
	ctx := context.Background()
	a, b := pgtest.Open(t, dbURL), pgtest.Open(t, dbURL)

	const check = 20 * time.Millisecond
	lock, err := a.AcquireInstanceLock(ctx, check)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if _, err := b.Pool().Exec(ctx, `
		SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND classid = 0 AND objid = 7272027 AND objsubid = 1`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lock.Lost():
	case <-time.After(50 * check):
		t.Fatal("lost lock not noticed")
	}
	other, err := b.AcquireInstanceLock(ctx, time.Hour)
	if err != nil {
		t.Fatalf("taking the freed lock: %v", err)
	}
	other.Release()
}

// TestEditedMigrationIsRefused checks that a migration whose recorded
// checksum differs from the file is refused, and that a missing checksum
// (from before checksums) is backfilled.
func TestEditedMigrationIsRefused(t *testing.T) {
	db := pgtest.Open(t, pgtest.URL(t, "postgres_test"))
	ctx := context.Background()

	if _, err := db.Pool().Exec(ctx, "UPDATE schema_migrations SET checksum = NULL WHERE version = '0001_users'"); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pgtest.Quiet); err != nil {
		t.Fatalf("backfilling: %v", err)
	}
	var sum *string
	if err := db.Pool().QueryRow(ctx, "SELECT checksum FROM schema_migrations WHERE version = '0001_users'").Scan(&sum); err != nil || sum == nil || len(*sum) != 64 {
		t.Fatalf("checksum not backfilled: %v %v", sum, err)
	}

	if _, err := db.Pool().Exec(ctx, "UPDATE schema_migrations SET checksum = 'edited' WHERE version = '0001_users'"); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pgtest.Quiet); !errors.Is(err, postgres.ErrMigrationEdited) {
		t.Fatalf("edited migration: got %v, want ErrMigrationEdited", err)
	}
}

// TestMigrationsAreIdempotent checks that migrating an up-to-date database is
// a no-op.
func TestMigrationsAreIdempotent(t *testing.T) {
	db := pgtest.Open(t, pgtest.URL(t, "postgres_test"))
	if err := db.Migrate(context.Background(), pgtest.Quiet); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}
