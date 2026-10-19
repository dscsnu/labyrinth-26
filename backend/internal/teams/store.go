package teams

import (
	"context"
	"crypto/rand"
	"encoding/json"
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
		e.cur.Store(&fresh)
	}
	s.mu.Unlock()

	var removed []uuid.UUID
	for _, m := range old.Members {
		if !slices.ContainsFunc(fresh.Members, func(f Member) bool { return f.ID == m.ID }) {
			removed = append(removed, m.ID)
		}
	}
	if e.gone {
		for _, id := range removed {
			s.hub.Send(id, noTeamEvent)
		}
	} else {
		s.publish(&fresh, removed...)
	}
	return writeErr
}

// noTeamEvent tells a user they're not in a team (they left, or it was
// deleted). Its counterpart, "team", carries the full team.
var noTeamEvent = live.NewEvent("no_team", []byte("{}"))

// teamEvent encodes t as a "team" live event, same JSON as GET /api/teams/me.
func teamEvent(t *Team) (live.Event, error) {
	data, err := json.Marshal(t)
	if err != nil {
		return live.Event{}, err
	}
	return live.NewEvent("team", data), nil
}

// publish sends t to its members and no_team to users who just left. Called
// after a write is applied, with the team's lock held, so each team's events
// go out in order.
func (s *Store) publish(t *Team, removed ...uuid.UUID) {
	ev, err := teamEvent(t)
	if err != nil {
		s.log.Error("encode team event", "team_id", t.ID, "error", err)
		return
	}
	for _, m := range t.Members {
		s.hub.Send(m.ID, ev)
	}
	for _, id := range removed {
		s.hub.Send(id, noTeamEvent)
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
