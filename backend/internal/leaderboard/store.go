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
//	  mu    Mutex   guards last
//	  last  []byte  the last broadcast Board, as JSON

// Rules are the leaderboard's settings from config.toml.
type Rules struct {
	// Size is how many teams the "leaderboard" event and GET carry
	// (leaderboard.size).
	Size int
	// PublishEvery is how often Run checks whether the top changed
	// (leaderboard.publish_every): the most often it's broadcast.
	PublishEvery time.Duration
}

// Leaderboard is the leaderboard use case.
type Leaderboard struct {
	standings Standings
	teams     Teams
	hub       *live.Hub
	log       *slog.Logger
	rules     Rules

	mu   sync.Mutex
	last []byte
}

// New returns the leaderboard over s and t, broadcasting through hub.
func New(s Standings, t Teams, hub *live.Hub, log *slog.Logger, rules Rules) *Leaderboard {
	return &Leaderboard{standings: s, teams: t, hub: hub, log: log, rules: rules}
}

// Updated is the "leaderboard" event's type when the top changed.
const Updated live.EventType = "updated"

// Refresh ranks the teams and broadcasts the top as a "leaderboard" event
// of type updated if it differs from the last broadcast. Reports whether it
// broadcast. Run calls it on a timer; tests call it directly.
func (l *Leaderboard) Refresh() bool {
	b := l.board(l.rank())
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
