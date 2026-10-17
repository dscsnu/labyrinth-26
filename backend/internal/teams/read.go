package teams

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/platform/live"
)

// Status is where a team is in the event.
type Status string

const (
	// Lobby means the team is forming: members can join, leave and ready up.
	Lobby Status = "LOBBY"
	// InGame means the team has started and its roster is frozen.
	InGame Status = "IN_GAME"
	// Finished means the team is done playing. Nothing sets it yet.
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

// Snapshot is userID's current team as a live event (team or no_team), sent
// first on a new event stream.
func (s *Store) Snapshot(userID uuid.UUID) live.Event {
	t, err := s.ForUser(userID)
	if err != nil {
		return noTeamEvent
	}
	ev, err := teamEvent(&t)
	if err != nil {
		s.log.Error("encode team event", "error", err)
		return noTeamEvent
	}
	return ev
}

// Size is how many members a team needs to start, and the most it can hold.
func (s *Store) Size() int { return s.rules.Size }
