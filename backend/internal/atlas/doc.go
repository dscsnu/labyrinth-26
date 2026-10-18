// Package atlas is the game's map: the waypoints (QR locations), the shapes a
// team can select, the patterns (sets of waypoints) that form each shape, and
// the spell each pattern casts when completed. It's static content, loaded
// from Postgres once at startup, validated, and held in memory; nothing
// changes it while the backend runs, so every lookup is lock-free. Other use
// cases (teamstate) read it through small interfaces of their own.
//
// See README.md for every rule and case, and how each file works.
package atlas
