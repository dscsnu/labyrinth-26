// Package tests holds the black-box tests of internal/leaderboard: the
// ranking and its tie-breaks, the caller's entry, and broadcasting only on a
// change, against fakes of teamstate and teams. The route and the event end
// to end are tested in internal/server/tests.
//
// Run with: go test ./internal/leaderboard/...
package tests
