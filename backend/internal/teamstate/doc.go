// Package teamstate tracks what each team does in the game: the shape it
// selected, its stack of scans toward a pattern of that shape, the waypoints
// it has visited, and the patterns and shapes it has completed, against the
// static game content. All of it is held in memory and every change is
// written through to Postgres before it is applied.
//
// See README.md for every rule and case, and how each file works.
package teamstate
