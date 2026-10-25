// Package activity is the admin live view's feed (/admin/live): every change
// to any team's game state, as it happens, for organisers. It stores
// nothing: team state hands it each change it publishes (Record), it turns
// that into a small entry, and a background loop sends what accumulated to
// the admins watching as one "activity" event every activity.publish_every.
// Memory is bounded: the queue between flushes and the recent entries a new
// view starts with are fixed-size rings. It also carries the full
// leaderboard to watching admins (SendAdmins), for the leaderboard use case.
//
// See README.md for every rule and case, and how each file works.
package activity
