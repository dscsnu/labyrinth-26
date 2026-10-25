package activity

import (
	"time"

	"labyrinth/internal/teams"
)

// Teams is what activity needs from the teams use case: a team's name and
// its members' names, to say who did what. Read from teams' memory.
type Teams interface {
	// ByID returns the team with that ID, and false if there's none.
	ByID(teamID string) (teams.Team, bool)
}

// Entry is one change to one team's game state, as the live view shows it.
// Only the fields its type uses are set; the rest are omitted from the JSON.
type Entry struct {
	// At is when the feed recorded it (the server's clock), to the millisecond.
	At time.Time `json:"at"`
	// TeamID is the team's 6-digit code.
	TeamID string `json:"team_id"`
	// Team is the team's name.
	Team string `json:"team"`
	// Type is the cause: the team_state event's type (shape_selected,
	// scan_added, wrong_scan, spell_cast, boss_defeated, boss_attacked,
	// knocked_out, revived, powerup_granted, powerup_activated, qte_answered,
	// …).
	Type string `json:"type"`
	// By is the name of the member who acted, or null for what the server
	// does by itself (an attack, a cooldown ending) or when it isn't known.
	By *string `json:"by"`
	// Shape is the shape selected or cast.
	Shape string `json:"shape,omitempty"`
	// Waypoint is the waypoint scanned.
	Waypoint string `json:"waypoint,omitempty"`
	// Dealt is the damage a cast dealt to a boss.
	Dealt *int `json:"dealt,omitempty"`
	// Boss is the serial of the boss a cast hit, or that attacked.
	Boss *int `json:"boss,omitempty"`
	// Health is the team's health after an attack or a revive.
	Health *int `json:"health,omitempty"`
	// Powerup is the powerup granted or activated.
	Powerup string `json:"powerup,omitempty"`
	// Correct reports whether a quiz answer was right.
	Correct *bool `json:"correct,omitempty"`
}

// Batch is the "activity" event's message: entries oldest first, and how
// many more were dropped (the queue holds activity.batch_max between
// flushes). A snapshot's Dropped is always 0.
type Batch struct {
	// Entries are the changes, oldest first; [] when none.
	Entries []Entry `json:"entries"`
	// Dropped is how many changes since the last flush aren't in Entries.
	Dropped int `json:"dropped"`
}

// Rules are activity's settings from config.toml, and its clock.
type Rules struct {
	// PublishEvery is how often Run flushes the queue to watching admins
	// (activity.publish_every).
	PublishEvery time.Duration
	// BatchMax is the queue's size between flushes (activity.batch_max).
	BatchMax int
	// Recent is how many of the latest entries a snapshot carries
	// (activity.recent).
	Recent int
	// Now is the feed's clock; nil means time.Now. Tests pass a fixed one.
	// Not in config.toml.
	Now func() time.Time
}

// Snapshot returns the "activity" event a watching admin's stream opens
// with (type snapshot): the latest activity.recent flushed entries, oldest
// first. Entries still queued arrive in the next batch.
func (f *Feed) Snapshot() Batch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Batch{Entries: f.recent.items(), Dropped: 0}
}
