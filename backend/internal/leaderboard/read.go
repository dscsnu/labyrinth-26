package leaderboard

import (
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
)

// Standings is what the leaderboard needs from teamstate: every team's
// progress, read from memory.
type Standings interface {
	Standings() []teamstate.Standing
}

// Teams is what the leaderboard needs from teams: names and statuses, and
// the caller's team.
type Teams interface {
	All() []teams.Team
	ForUser(userID uuid.UUID) (teams.Team, error)
}

// Entry is one team's place on the leaderboard.
type Entry struct {
	// Rank is its place, from 1. Every team has its own rank: ties break
	// by team ID, so the order is the same for everyone.
	Rank int `json:"rank"`
	// TeamID and Name identify the team.
	TeamID string `json:"team_id"`
	Name   string `json:"name"`
	// BossesDefeated is how many bosses it has defeated: the first key.
	BossesDefeated int `json:"bosses_defeated"`
	// LastDefeatAt is when its latest boss fell, the second key (earlier
	// ranks higher), or null if none has.
	LastDefeatAt *time.Time `json:"last_defeat_at"`
	// Damage is what it has dealt to its current boss: the third key.
	Damage int `json:"damage"`
	// Finished reports that it has defeated every boss (FINISHED).
	Finished bool `json:"finished"`
}

// Board is the top of the leaderboard: the "leaderboard" event's message.
type Board struct {
	// Top is the first leaderboard.size teams, best first.
	Top []Entry `json:"top"`
	// Teams is how many teams are ranked (started or finished).
	Teams int `json:"teams"`
}

// Mine is GET /api/leaderboard's answer: the top, and the caller's team.
type Mine struct {
	Board
	// Me is the caller's team's entry, or null if they have no started team.
	Me *Entry `json:"me"`
}

// rank returns every started or finished team, ranked: bosses defeated
// (more first), then the latest defeat's time (earlier first; a team with
// none after one with some), then damage on the current boss (more first),
// then team ID.
func (l *Leaderboard) rank() []Entry {
	progress := map[string]teamstate.Standing{}
	for _, sd := range l.standings.Standings() {
		progress[sd.TeamID] = sd
	}
	var out []Entry
	for _, t := range l.teams.All() {
		if t.Status == teams.Lobby {
			continue
		}
		sd := progress[t.ID] // zero for a team that hasn't done anything
		e := Entry{TeamID: t.ID, Name: t.Name, BossesDefeated: sd.BossesDefeated, Damage: sd.Damage, Finished: t.Status == teams.Finished}
		if !sd.LastDefeatAt.IsZero() {
			at := sd.LastDefeatAt.UTC()
			e.LastDefeatAt = &at
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b Entry) int {
		if a.BossesDefeated != b.BossesDefeated {
			return b.BossesDefeated - a.BossesDefeated
		}
		switch {
		case a.LastDefeatAt != nil && b.LastDefeatAt == nil:
			return -1
		case a.LastDefeatAt == nil && b.LastDefeatAt != nil:
			return 1
		case a.LastDefeatAt != nil && !a.LastDefeatAt.Equal(*b.LastDefeatAt):
			return a.LastDefeatAt.Compare(*b.LastDefeatAt)
		}
		if a.Damage != b.Damage {
			return b.Damage - a.Damage
		}
		return strings.Compare(a.TeamID, b.TeamID)
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

// board returns the top leaderboard.size of ranked.
func (l *Leaderboard) board(ranked []Entry) Board {
	top := ranked[:min(len(ranked), l.rules.Size)]
	return Board{Top: append([]Entry{}, top...), Teams: len(ranked)}
}

// ForUser returns the top of the leaderboard and userID's team's entry
// (nil if they have no started team), ranked now from memory.
func (l *Leaderboard) ForUser(userID uuid.UUID) Mine {
	ranked := l.rank()
	m := Mine{Board: l.board(ranked)}
	if t, err := l.teams.ForUser(userID); err == nil {
		for i := range ranked {
			if ranked[i].TeamID == t.ID {
				e := ranked[i]
				m.Me = &e
				break
			}
		}
	}
	return m
}
