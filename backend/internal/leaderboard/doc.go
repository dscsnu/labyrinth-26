// Package leaderboard ranks the started teams: by bosses defeated, then who
// got there first, then damage on the current boss. It stores nothing: the
// ranking is derived from team state's standings and the teams' names when
// asked (GET /api/leaderboard), and a background loop broadcasts the top of
// it as a game-wide "leaderboard" event whenever that changes, at most once
// every leaderboard.publish_every.
//
// See README.md for every rule and case, and how each file works.
package leaderboard
