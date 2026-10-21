package teams

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/platform/live"
)

// Status is where a team is in the event.
type Status string

const (
	// Lobby means the team is forming: members can join, leave and ready up.
	Lobby Status = "LOBBY"
	// InGame means the team has started: players can't join or leave, and
	// only an admin can remove a member (RemoveMember).
	InGame Status = "IN_GAME"
	// Finished means the team is done playing: it defeated the last boss
	// (Finish, called by teamstate). Its game writes are refused (not in
	// game), and its members stay in it, so they can't join another team.
	Finished Status = "FINISHED"
)

// Member is one player in a team.
type Member struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	AvatarURL *string   `json:"avatar_url"`
	IsReady   bool      `json:"is_ready"`
	JoinedAt  time.Time `json:"joined_at"`
}

// Team is one immutable snapshot. Members are in join order, which decides
// who becomes leader if the leader leaves.
type Team struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	LeaderID  uuid.UUID  `json:"leader_id"`
	Status    Status     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at"`
	Members   []Member   `json:"members"`
	// Version orders this team's snapshots (see live.Version): Seq goes up
	// with every change to the team. Memory-only; it starts again, in a new
	// Epoch, after a restart.
	Version live.Version `json:"version"`
}

// ErrNotInTeam means the user isn't in any team.
var ErrNotInTeam = errors.New("not in a team")

// All reads come from memory: two map lookups under a brief shared lock, then
// a lock-free load of the team's snapshot.

// ForUser returns userID's team.
func (s *Store) ForUser(userID uuid.UUID) (Team, error) {
	s.mu.RLock()
	e := s.teams[s.userTeam[userID]]
	s.mu.RUnlock()
	if e == nil {
		return Team{}, ErrNotInTeam
	}
	return *e.cur.Load(), nil
}

// All returns every team in memory, sorted by ID: for admin views, and for
// comparing memory across a restart. It holds the shared lock while it copies
// the index, then loads each snapshot lock-free.
func (s *Store) All() []Team {
	s.mu.RLock()
	entries := make([]*entry, 0, len(s.teams))
	for _, e := range s.teams {
		entries = append(entries, e)
	}
	s.mu.RUnlock()
	out := make([]Team, 0, len(entries))
	for _, e := range entries {
		out = append(out, *e.cur.Load())
	}
	slices.SortFunc(out, func(a, b Team) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// ByID returns a team by its ID.
func (s *Store) ByID(teamID string) (Team, bool) {
	s.mu.RLock()
	e := s.teams[teamID]
	s.mu.RUnlock()
	if e == nil {
		return Team{}, false
	}
	return *e.cur.Load(), true
}

// Snapshot is userID's current team as a live event (team or no_team, of
// type snapshot), sent first on a new event stream.
func (s *Store) Snapshot(userID uuid.UUID) live.Event {
	t, err := s.ForUser(userID)
	if err != nil {
		return noTeamSnapshot
	}
	ev, err := teamEvent(&t, live.Snapshot)
	if err != nil {
		s.log.Error("encode team event", "error", err)
		return noTeamSnapshot
	}
	return ev
}

// Size is the most members a team can hold (teams.size); a team can start
// with any number from 1 to Size.
func (s *Store) Size() int { return s.rules.Size }
