package teams

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres"
)

// The in-memory layout. Every team is in memory; reads never touch Postgres.
//
//	Store.mu (RWMutex) guards the two indexes:
//	  teams     map[teamID]*entry      "give me that team"
//	  userTeam  map[userID]teamID      "which team is this user in?"
//	entry
//	  mu   Mutex                       writers to this one team queue here
//	  cur  atomic.Pointer[Team]        the current snapshot; readers load it lock-free
//
// Snapshots are never modified: a write builds a new Team and swaps the
// pointer. Lock order: entry.mu before Store.mu. Store.mu is never held during
// database I/O.

// entry holds one team: its write lock and its current snapshot.
type entry struct {
	mu   sync.Mutex
	cur  atomic.Pointer[Team]
	gone bool // deleted; guarded by mu
}

// Store is the teams use case: in-memory team state, written through to
// Postgres.
type Store struct {
	rules Rules
	db    db
	hub   *live.Hub
	log   *slog.Logger

	mu       sync.RWMutex
	teams    map[string]*entry
	userTeam map[uuid.UUID]string
}

// Rules are the team settings from config.toml.
type Rules struct {
	// Size is how many members a team needs before the leader can start, and
	// the most it can hold (teams.size).
	Size int
	// NameMaxLength is the longest team name, in characters
	// (teams.name_max_length).
	NameMaxLength int
	// WriteTimeout bounds each write-through (storage.write_timeout).
	WriteTimeout time.Duration
	// CodeAttempts is how many random team codes Create tries
	// (teams.code_attempts).
	CodeAttempts int
	// OnStart, if not nil, is called by Start once the team is IN_GAME in
	// memory and its "team" event is published, with the team's lock still
	// held. server.Build passes teamstate's PublishTeam, so every member gets
	// the team's state right after the team itself (AGENTS.md §4: teams
	// doesn't import teamstate). It must not call back into this team's
	// writes. Not in config.toml.
	OnStart func(Team)
	// Epoch is the backend process's epoch for live.Version, the same for
	// every use case: server.Build sets it once at startup. Not in
	// config.toml.
	Epoch int64
	// OnRosterChange, if not nil, is called by RemoveMember when it removes a
	// member from a team that has started, once the team is in memory and
	// published, with the team's lock still held. server.Build passes
	// teamstate's RosterChanged, so team state stops sending to the removed
	// user and refuses their writes. Same constraints as OnStart. Not in
	// config.toml.
	OnRosterChange func(Team)
}

// lockRetries bounds lockUsersTeam's retries when the user changes team
// between looking up their team and locking it. It only matters for one
// user's own concurrent requests, so it's a correctness bound, not a tunable.
const lockRetries = 3

// Load builds the store from Postgres, enforcing rules. Call once at startup.
func Load(ctx context.Context, pg *postgres.DB, hub *live.Hub, log *slog.Logger, rules Rules) (*Store, error) {
	d := db{pool: pg.Pool()}
	all, err := d.selectAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("load teams: %w", err)
	}
	s := &Store{
		rules:    rules,
		db:       d,
		hub:      hub,
		log:      log,
		teams:    make(map[string]*entry, len(all)),
		userTeam: make(map[uuid.UUID]string),
	}
	for i := range all {
		all[i].Version = live.Version{Epoch: rules.Epoch}
		e := &entry{}
		e.cur.Store(&all[i])
		s.teams[all[i].ID] = e
		for _, m := range all[i].Members {
			s.userTeam[m.ID] = all[i].ID
		}
	}
	log.Info("loaded teams", "teams", len(all), "players", len(s.userTeam))
	return s, nil
}

// persist is the write-through step: it runs a db.go write detached from the
// request (a client disconnecting mid-commit must not leave memory unsure
// whether the database committed) and bounded by Rules.WriteTimeout.
func (s *Store) persist(ctx context.Context, write func(context.Context) error) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.rules.WriteTimeout)
	defer cancel()
	return write(wctx)
}

// lockTeam finds teamID and locks it; the caller unlocks e.mu.
// ErrTeamNotFound if there's no such team, or it was deleted between lookup
// and lock.
func (s *Store) lockTeam(teamID string) (*entry, *Team, error) {
	s.mu.RLock()
	e := s.teams[teamID]
	s.mu.RUnlock()
	if e == nil {
		return nil, nil, ErrTeamNotFound
	}
	e.mu.Lock()
	if e.gone {
		e.mu.Unlock()
		return nil, nil, ErrTeamNotFound
	}
	return e, e.cur.Load(), nil
}

// lockUsersTeam finds userID's team and locks it; the caller unlocks e.mu.
// Retries up to lockRetries times if the user's team changed between lookup
// and lock.
func (s *Store) lockUsersTeam(userID uuid.UUID) (*entry, *Team, error) {
	for range lockRetries {
		s.mu.RLock()
		teamID, ok := s.userTeam[userID]
		e := s.teams[teamID]
		s.mu.RUnlock()
		if !ok || e == nil {
			return nil, nil, ErrNotInTeam
		}

		e.mu.Lock()
		t := e.cur.Load()
		if !e.gone && slices.ContainsFunc(t.Members, func(m Member) bool { return m.ID == userID }) {
			return e, t, nil
		}
		e.mu.Unlock()
	}
	return nil, nil, ErrNotInTeam
}

// inTeam reports whether userID is in any team.
func (s *Store) inTeam(userID uuid.UUID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.userTeam[userID]
	return ok
}

// reconcile runs after a failed write-through, with e.mu held. The write may
// or may not have committed (e.g. a timeout during COMMIT), so the team is
// re-read from Postgres, which wins on disagreement. Returns writeErr.
func (s *Store) reconcile(e *entry, teamID string, writeErr error) error {
	s.log.Error("team write-through failed; reloading team from database", "team_id", teamID, "error", writeErr)

	ctx, cancel := context.WithTimeout(context.Background(), s.rules.WriteTimeout)
	defer cancel()
	fresh, err := s.db.selectOne(ctx, teamID)
	if err != nil && !errors.Is(err, postgres.ErrNotFound) {
		s.log.Error("reload after failed write also failed; memory may be stale until restart", "team_id", teamID, "error", err)
		return writeErr
	}

	old := e.cur.Load()
	s.mu.Lock()
	for _, m := range old.Members {
		if s.userTeam[m.ID] == teamID {
			delete(s.userTeam, m.ID)
		}
	}
	if errors.Is(err, postgres.ErrNotFound) {
		delete(s.teams, teamID)
		e.gone = true
	} else {
		for _, m := range fresh.Members {
			s.userTeam[m.ID] = teamID
		}
		fresh.Version = old.Version.Next(s.rules.Epoch) // never back, even on a reload
		e.cur.Store(&fresh)
	}
	s.mu.Unlock()

	var removed []uuid.UUID
	for _, m := range old.Members {
		if !slices.ContainsFunc(fresh.Members, func(f Member) bool { return f.ID == m.ID }) {
			removed = append(removed, m.ID)
		}
	}
	if !e.gone {
		s.publish(&fresh, live.Resync)
	}
	s.sendNoTeam(noTeamResync, removed...)
	return writeErr
}

// The causes of "team" events (the envelope's type), besides live.Snapshot
// and live.Resync. Each is published to the team's members after the
// change; the message is always the whole team.
const (
	// TeamCreated: the team was created; its creator is the only member.
	TeamCreated live.EventType = "created"
	// TeamMemberJoined: a player joined.
	TeamMemberJoined live.EventType = "member_joined"
	// TeamMemberLeft: a member left; the leader didn't change.
	TeamMemberLeft live.EventType = "member_left"
	// TeamLeaderChanged: the leader left, or an admin removed them, and
	// another member now leads (leader_id).
	TeamLeaderChanged live.EventType = "leader_changed"
	// TeamMemberRemoved: an admin removed a member who wasn't the leader.
	TeamMemberRemoved live.EventType = "member_removed"
	// TeamReadyChanged: a member's ready flag changed.
	TeamReadyChanged live.EventType = "ready_changed"
	// TeamStarted: the leader started the game; the team is IN_GAME.
	TeamStarted live.EventType = "started"
	// TeamProfileUpdated: a member signed in with a new name or avatar.
	TeamProfileUpdated live.EventType = "profile_updated"
)

// The causes of "no_team" events, besides live.Snapshot and live.Resync.
// Each goes to the one user who is no longer in a team.
const (
	// NoTeamLeft: the user left; the team carries on without them.
	NoTeamLeft live.EventType = "left"
	// NoTeamDeleted: the user was the last member, so leaving deleted the
	// team.
	NoTeamDeleted live.EventType = "team_deleted"
	// NoTeamRemoved: an admin removed the user from their team.
	NoTeamRemoved live.EventType = "removed"
)

// The "no_team" events, one per type. Their message is always {}, so each is
// encoded once.
var (
	// noTeamSnapshot opens the stream of a user in no team.
	noTeamSnapshot = noTeamEvent(live.Snapshot)
	// noTeamResync goes to users a reload after a failed write found
	// outside the team.
	noTeamResync = noTeamEvent(live.Resync)
	// noTeamLeft goes to a user who left a team that carries on.
	noTeamLeft = noTeamEvent(NoTeamLeft)
	// noTeamDeleted goes to the last member, whose leaving deleted the team.
	noTeamDeleted = noTeamEvent(NoTeamDeleted)
	// noTeamRemoved goes to a user an admin removed.
	noTeamRemoved = noTeamEvent(NoTeamRemoved)
)

// noTeamEvent builds the "no_team" event of type typ: the user is in no
// team. Its counterpart, "team", carries the full team.
func noTeamEvent(typ live.EventType) live.Event {
	return live.MustNewEvent("no_team", typ, struct{}{})
}

// teamEvent encodes t as a "team" live event of type typ; its message is the
// same JSON as GET /api/teams/me.
func teamEvent(t *Team, typ live.EventType) (live.Event, error) {
	return live.NewEvent("team", typ, t)
}

// publish sends t to its members as a "team" event of type typ. Called
// after a write is applied, with the team's lock held, so each team's events
// go out in order.
func (s *Store) publish(t *Team, typ live.EventType) {
	ev, err := teamEvent(t, typ)
	if err != nil {
		s.log.Error("encode team event", "team_id", t.ID, "error", err)
		return
	}
	for _, m := range t.Members {
		s.hub.Send(m.ID, ev)
	}
}

// sendNoTeam sends the "no_team" event ev to each of users, who just stopped
// being in a team. Called with the team's lock held, like publish.
func (s *Store) sendNoTeam(ev live.Event, users ...uuid.UUID) {
	for _, id := range users {
		s.hub.Send(id, ev)
	}
}

// randomTeamID returns a random team code between teamCodeMin and
// teamCodeMax.
func randomTeamID() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(teamCodeMax-teamCodeMin+1))
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(n.Int64()+teamCodeMin, 10), nil
}

// teamCodeMin and teamCodeMax bound team codes: always 6 digits, matching the
// team.id CHECK in migration 0002 and teamIDPattern. A format, not a tunable.
const (
	// teamCodeMin is the smallest team code.
	teamCodeMin = 100_000
	// teamCodeMax is the largest team code.
	teamCodeMax = 999_999
)
