// Package admin is the organisers' toolbox: every /api/admin/… route, in one
// place. It owns no data and no state: each route checks its input and calls
// the use case that owns the data (teams, teamstate, user), through small
// interfaces declared here, which server.Build fills with the real stores.
// Only admins reach it: server.Build wraps every route in RequireAuth and
// user's RequireAdmin, which reads the role from Postgres on every request.
//
// See README.md for every rule and case, and how each file works.
package admin
