// Package user stores accounts: one per Google subject ID, with a role. It
// keeps nothing in memory; reads and writes go straight to Postgres.
//
// See README.md for every rule and case, and how each file works.
package user
