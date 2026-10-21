// Package qte owns quick-time events: game-wide events an admin starts that
// every player sees at once, a quiz being the first kind. It holds every QTE
// in memory, written through to Postgres (tables qte), runs one at a time,
// broadcasts the open one as the game-wide "qte" event, and closes it on a
// timer, revealing the answer. Teams' answers are teamstate's (they're
// per-team results, in team_state), which reads QTEs through ByID.
//
// See README.md for every rule and case, and how each file works.
package qte
