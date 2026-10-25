package activity

import (
	"slices"

	"github.com/google/uuid"

	"labyrinth/internal/platform/live"
	"labyrinth/internal/teamstate"
)

// Record turns one published change to a team's state into an entry and
// queues it for the next flush, dropping (and counting) the oldest queued
// one if the queue is full. Team state calls it from its publish, holding
// that team's entry lock, so it does no I/O and no encoding: it reads the
// team's names from teams' memory and appends under the feed's mutex.
// snapshot and resync are resends, not changes: they're ignored.
func (f *Feed) Record(teamID string, typ live.EventType, v teamstate.View) {
	if typ == live.Snapshot || typ == live.Resync {
		return
	}
	t, _ := f.teams.ByID(teamID)
	name := func(id uuid.UUID) *string {
		for _, m := range t.Members {
			if m.ID == id {
				return &m.Name
			}
		}
		return nil
	}
	e := Entry{At: f.now().UTC().Truncate(1e6), TeamID: teamID, Team: t.Name, Type: string(typ)}
	switch typ {
	case teamstate.ShapeSelected:
		if s := v.Selection; s != nil {
			e.Shape, e.By = s.Shape, name(s.SelectedBy)
		}
	case teamstate.ScanAdded, teamstate.WrongScan:
		if n := len(v.Stack); n > 0 {
			e.Waypoint, e.By = v.Stack[n-1].WaypointID, name(v.Stack[n-1].UserID)
		}
	case teamstate.SpellCast, teamstate.BossDefeated:
		if c := v.LastCast; c != nil {
			dealt := c.Dealt
			e.Shape, e.Dealt, e.Boss, e.By = c.Shape, &dealt, c.Boss, name(c.CastBy)
		}
	case teamstate.BossAttacked, teamstate.KnockedOut, teamstate.Revived:
		health := v.Health
		e.Health = &health
		if v.Boss != nil {
			serial := v.Boss.Serial
			e.Boss = &serial
		}
	case teamstate.QTEAnswered:
		if q := v.QTE; q != nil {
			correct := q.Correct
			e.Correct, e.By = &correct, name(q.AnsweredBy)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// Which powerup: the one held now and not before (granted), or held
	// before and not now (activated). Every publish updates the record.
	before := f.held[teamID]
	switch typ {
	case teamstate.PowerupGranted:
		e.Powerup = firstMissing(v.Powerups.Held, before)
	case teamstate.PowerupActivated:
		e.Powerup = firstMissing(before, v.Powerups.Held)
	}
	f.held[teamID] = slices.Clone(v.Powerups.Held)
	if f.pending.push(e) {
		f.dropped++
	}
}

// firstMissing returns the first of a not in b, or "".
func firstMissing(a, b []string) string {
	for _, k := range a {
		if !slices.Contains(b, k) {
			return k
		}
	}
	return ""
}

// Watch adds userID to the admins the feed and the full leaderboard go to,
// until the returned stop is called (once per stream: an admin with two
// live views open is counted twice). The caller has checked the role.
func (f *Feed) Watch(userID uuid.UUID) (stop func()) {
	f.mu.Lock()
	f.watchers[userID]++
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.watchers[userID]--; f.watchers[userID] <= 0 {
			delete(f.watchers, userID)
		}
	}
}

// Watching reports how many admins are watching.
func (f *Feed) Watching() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.watchers)
}

// SendAdmins sends ev to every watching admin's streams. The leaderboard
// sends its full ranking through it.
func (f *Feed) SendAdmins(ev live.Event) {
	for _, id := range f.admins() {
		f.hub.Send(id, ev)
	}
}

// admins returns the watching admins' IDs.
func (f *Feed) admins() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uuid.UUID, 0, len(f.watchers))
	for id := range f.watchers {
		out = append(out, id)
	}
	return out
}

// Flush moves the queued entries to the recent ring and, if there were any
// (or any were dropped), sends them to every watching admin as one
// "activity" event of type batch. Reports whether it sent one. Run calls it
// on a timer; tests call it directly. Nothing is sent, and the queue is
// still emptied, when nobody watches.
func (f *Feed) Flush() bool {
	f.mu.Lock()
	b := Batch{Entries: f.pending.items(), Dropped: f.dropped}
	for _, e := range b.Entries {
		f.recent.push(e)
	}
	f.pending.clear()
	f.dropped = 0
	f.mu.Unlock()
	if len(b.Entries) == 0 && b.Dropped == 0 {
		return false
	}
	admins := f.admins()
	if len(admins) == 0 {
		return false
	}
	ev, err := live.NewEvent("activity", Batched, b)
	if err != nil {
		f.log.Error("encode activity batch", "error", err)
		return false
	}
	for _, id := range admins {
		f.hub.Send(id, ev)
	}
	return true
}
