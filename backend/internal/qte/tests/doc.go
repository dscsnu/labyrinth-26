// Package tests holds the black-box tests of internal/qte. They use only
// qte's exported API, against real Postgres and a fake clock. Tests that need
// Postgres use TEST_DATABASE_URL and skip without it.
//
// Run with: go test ./internal/qte/...
package tests
