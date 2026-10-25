package leaderboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"labyrinth/internal/platform/live"
)

// The in-memory layout: nothing but what was last broadcast, to tell
// whether the top changed. The ranking itself is derived each time.
//
//	Leaderboard
//	  mu        Mutex   guards last and lastFull
//	  last      []byte  the last broadcast Board, as JSON
//	  lastFull  []byte  the last full ranking sent to admins, as JSON

// Rules are the leaderboard's settings from config.toml.
type Rules struct {
	// Size is how many teams the "leaderboard" event and GET carry
	// (leaderboard.size).
	Size int
	// PublishEvery is how often Run checks whether the top changed
	// (leaderboard.publish_every): the most often it's broadcast.
	PublishEvery time.Duration
	// SendAdmins, when set, sends an event to the admins watching the live
	// view (server.Build points it at activity.Feed.SendAdmins): Refresh
	// sends them the full ranking through it. Not in config.toml.
	SendAdmins func(live.Event)
}

// Leaderboard is the leaderboard use case.
type Leaderboard struct {
	standings Standings
	teams     Teams
	hub       *live.Hub
	log       *slog.Logger
	rules     Rules

	mu       sync.Mutex
	last     []byte
	lastFull []byte
}

// New returns the leaderboard over s and t, broadcasting through hub.
func New(s Standings, t Teams, hub *live.Hub, log *slog.Logger, rules Rules) *Leaderboard {
	return &Leaderboard{standings: s, teams: t, hub: hub, log: log, rules: rules}
}

// Updated is the "leaderboard" event's type when the top changed.
const Updated live.EventType = "updated"

// Refresh ranks the teams and broadcasts the top as a "leaderboard" event
// of type updated if it differs from the last broadcast. Reports whether it
// broadcast. With Rules.SendAdmins set it also sends the full ranking to the
// watching admins as "admin_leaderboard" (type updated) when that changed
// (refreshFull). Run calls it on a timer; tests call it directly.
func (l *Leaderboard) Refresh() bool {
	ranked := l.rank()
	l.refreshFull(ranked)
	b := l.board(ranked)
	data, err := json.Marshal(b)
	if err != nil {
		l.log.Error("encode leaderboard", "error", err)
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if string(data) == string(l.last) {
		return false
	}
	ev, err := live.NewEvent("leaderboard", Updated, b)
	if err != nil {
		l.log.Error("encode leaderboard event", "error", err)
		return false
	}
	l.last = data
	l.hub.Broadcast(ev) // under mu, so broadcasts go out in order
	return true
}

// Full is the "admin_leaderboard" event's message: every started or
// finished team, ranked.
type Full struct {
	// Teams is the whole ranking; [] when no team has started.
	Teams []Entry `json:"teams"`
}

// refreshFull sends ranked to the watching admins (Rules.SendAdmins) if it
// differs from the last one sent. It encodes nothing when SendAdmins is
// unset.
func (l *Leaderboard) refreshFull(ranked []Entry) {
	if l.rules.SendAdmins == nil {
		return
	}
	full := Full{Teams: append([]Entry{}, ranked...)}
	data, err := json.Marshal(full)
	if err != nil {
		l.log.Error("encode full leaderboard", "error", err)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if string(data) == string(l.lastFull) {
		return
	}
	ev, err := live.NewEvent("admin_leaderboard", Updated, full)
	if err != nil {
		l.log.Error("encode full leaderboard event", "error", err)
		return
	}
	l.lastFull = data
	l.rules.SendAdmins(ev) // under mu, so admins get them in order
}

// AdminSnapshot is the "admin_leaderboard" event a watching admin's stream
// opens with (type snapshot): the full ranking as it is now.
func (l *Leaderboard) AdminSnapshot() (live.Event, error) {
	return live.NewEvent("admin_leaderboard", live.Snapshot, Full{Teams: append([]Entry{}, l.rank()...)})
}

// Run calls Refresh every leaderboard.publish_every until ctx is done.
// server.Build's caller (main) starts it.
func (l *Leaderboard) Run(ctx context.Context) {
	tick := time.NewTicker(l.rules.PublishEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			l.Refresh()
		}
	}
}

// Snapshot is the "leaderboard" event a stream opens with (type
// snapshot): the top as it is now.
func (l *Leaderboard) Snapshot() (live.Event, error) {
	return live.NewEvent("leaderboard", live.Snapshot, l.board(l.rank()))
}
