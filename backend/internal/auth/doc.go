// Package auth signs users in with Google, issues and verifies access tokens,
// and provides the middleware that protects /api/*. It keeps nothing in
// memory: tokens are stateless JWTs and login codes live in Postgres.
//
// See README.md for every rule and case, and how each file works.
package auth
