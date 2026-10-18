// Package tests holds the black-box tests of internal/user. They use only
// user's exported API, so they test what other packages can rely on. Tests that
// need Postgres use TEST_DATABASE_URL and skip without it.
//
// Run with: go test ./internal/user/...
package tests
