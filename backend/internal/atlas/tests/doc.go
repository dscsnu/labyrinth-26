// Package tests holds the black-box tests of internal/atlas. They use only
// atlas's exported API, so they test what other packages can rely on. Tests
// that need Postgres use TEST_DATABASE_URL and skip without it.
//
// Run with: go test ./internal/atlas/...
package tests
