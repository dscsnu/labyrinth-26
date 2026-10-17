// Package pgtest gives tests a migrated, empty Postgres schema of their own.
// Requires TEST_DATABASE_URL pointing at a disposable database; tests skip
// without it. Each package uses its own schema, so packages can run in
// parallel.
package pgtest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"labyrinth/internal/platform/postgres"
)

// Quiet is a logger that discards everything.
var Quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// URL wipes and recreates schema in the test database and returns a
// connection URL whose search_path is that schema.
func URL(t testing.TB, schema string) string {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %[1]s CASCADE; CREATE SCHEMA %[1]s", pgx.Identifier{schema}.Sanitize())); err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// Open connects to dbURL and applies migrations. The pool closes with the test.
func Open(t testing.TB, dbURL string) *postgres.DB {
	t.Helper()
	db, err := postgres.Connect(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(context.Background(), Quiet); err != nil {
		t.Fatal(err)
	}
	return db
}

// Must panics on error, failing the test with a stack trace. (Go can't pass a
// two-result call alongside a *testing.T argument.)
func Must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
