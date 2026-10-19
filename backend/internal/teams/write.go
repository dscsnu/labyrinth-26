package teams

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/platform/live"
)

// Writes. Every write follows the same write-through steps, marked in each
// function:
//
//	1. lock      the team's entry, so writes to one team are serialised
//	2. validate  the rules against the current in-memory snapshot
//	3. build     the next snapshot (a copy; the current one is never modified)
//	4. persist   to Postgres via db.go. If this fails, memory is reloaded from
//	             Postgres and nothing is applied
//	5. apply     swap in the next snapshot and update the indexes
//	6. publish   send the new state to members' live streams
//
// Because step 5 only runs after step 4 commits, memory never holds a change
// Postgres doesn't have, and a crash loses nothing that was acknowledged.
//
// The variations AGENTS.md §6 allows, labelled where they occur:
//   - Create has no 1. lock: the team has no entry until 5. apply makes it,
//     and Postgres's unique constraint on teammember.user_id catches a
//     concurrent create or join.
//   - Create, Join and Start run 4. persist before 3. build: Postgres
//     generates part of the new value (created_at, joined_at, started_at), so
//     the snapshot is built from what was stored.

var (
	// ErrTeamNotFound means no team has that ID.
	ErrTeamNotFound = errors.New("team not found")
	// ErrAlreadyInTeam means the user is already in a team.
	ErrAlreadyInTeam = errors.New("already in a team")
	// ErrTeamFull means the team already has Rules.Size members.
	ErrTeamFull = errors.New("team is full")
	// ErrTeamLocked means the team has started, so players can't join or
	// leave (an admin can still remove a member).
	ErrTeamLocked = errors.New("team is no longer in the lobby")
	// ErrNotLeader means only the leader may do this.
	ErrNotLeader = errors.New("only the team leader can do this")
	// ErrLeaderReady means the leader tried to ready up; starting is their ready.
	ErrLeaderReady = errors.New("the leader starts the game instead of readying up")
	// ErrNotAllReady means a non-leader member isn't ready.
	ErrNotAllReady = errors.New("not every member is ready")
	// ErrNotMember means the user isn't in that team (RemoveMember).
	ErrNotMember = errors.New("that user is not in this team")
	// ErrLastMember means removing the user would leave the team empty;
	// RemoveMember never deletes a team.
	ErrLastMember = errors.New("can't remove a team's only member")
	// ErrNewLeaderRequired means the removed user leads the team, so a
	// remaining member must be named as the new leader.
	ErrNewLeaderRequired = errors.New("removing the leader needs a new leader from the remaining members")
	// ErrInvalidNewLeader means the new leader isn't a remaining member, or
	// one was named when the removed user isn't the leader.
	ErrInvalidNewLeader = errors.New("the new leader must be a remaining member, and is only named when removing the leader")
)

// Create makes a new LOBBY team with userID as its leader and only member.
func (s *Store) Create(ctx context.Context, userID uuid.UUID, name string) (Team, error) {
	// 1. lock: none. A new team has no entry until 5. apply creates it;
	// Postgres's unique constraint on teammember.user_id catches a concurrent
	// create or join.

	// 2. validate
	if s.inTeam(userID) {
		return Team{}, ErrAlreadyInTeam
	}

	for range s.rules.CodeAttempts {
		id, err := randomTeamID()
		if err != nil {
			return Team{}, err
		}
		s.mu.RLock()
		_, taken := s.teams[id]
		s.mu.RUnlock()
		if taken {
			continue
		}

		// 4. persist, before 3. build: Postgres sets created_at and joined_at
		var team Team
		err = s.persist(ctx, func(ctx context.Context) (err error) {
			team, err = s.db.insertTeam(ctx, id, name, userID)
			return err
		})
		switch {
		case errors.Is(err, errTeamIDTaken):
			continue
		case errors.Is(err, errUserInTeam):
			return Team{}, ErrAlreadyInTeam
		case err != nil:
			return Team{}, err
		}

		// 3. build: the snapshot is the team as Postgres returned it, at its
		// first version
		team.Version = live.Version{}.Next(s.rules.Epoch)
		// 5. apply
		e := &entry{}
		e.cur.Store(&team)
		s.mu.Lock()
		s.teams[id] = e
		s.userTeam[userID] = id
		s.mu.Unlock()
		// 6. publish
		s.publish(&team, TeamCreated)
		return team, nil
	}
	return Team{}, errors.New("could not find a free team id")
}

// Join adds userID to a LOBBY team that has room.
func (s *Store) Join(ctx context.Context, userID uuid.UUID, teamID string) (Team, error) {
	// 1. lock
	s.mu.RLock()
	e := s.teams[teamID]
	s.mu.RUnlock()
	if e == nil {
		return Team{}, ErrTeamNotFound
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	// 2. validate
	if e.gone {
		return Team{}, ErrTeamNotFound
	}
	t := e.cur.Load()
	if t.Status != Lobby {
		return Team{}, ErrTeamLocked
	}
	if s.inTeam(userID) {
		return Team{}, ErrAlreadyInTeam
	}
	if len(t.Members) >= s.rules.Size {
		return Team{}, ErrTeamFull
	}

	// 4. persist, before 3. build: Postgres sets the new member's joined_at
	// and returns their profile
	var m Member
	err := s.persist(ctx, func(ctx context.Context) (err error) {
		m, err = s.db.insertMember(ctx, teamID, userID)
		return err
	})
	if errors.Is(err, errUserInTeam) {
		return Team{}, ErrAlreadyInTeam // joined another team concurrently; Postgres caught it
	}
	if err != nil {
		return Team{}, s.reconcile(e, teamID, err)
	}

	// 3. build
	next := *t
	next.Members = append(slices.Clone(t.Members), m)
	next.Version = t.Version.Next(s.rules.Epoch)
	// 5. apply
	e.cur.Store(&next)
	s.mu.Lock()
	s.userTeam[userID] = teamID
	s.mu.Unlock()
	// 6. publish
	s.publish(&next, TeamMemberJoined)
	return next, nil
}

// Leave removes userID from their LOBBY team. If they led it, the
// longest-standing remaining member becomes leader; if nobody is left, the
// team is deleted.
func (s *Store) Leave(ctx context.Context, userID uuid.UUID) error {
	// 1. lock
	e, t, err := s.lockUsersTeam(userID)
	if err != nil {
		return err
	}
	defer e.mu.Unlock()

	// 2. validate
	if t.Status != Lobby {
		return ErrTeamLocked
	}

	// 3. build
	next := *t
	next.Members = slices.DeleteFunc(slices.Clone(t.Members), func(m Member) bool { return m.ID == userID })
	deleteTeam := len(next.Members) == 0
	var newLeader *uuid.UUID
	if t.LeaderID == userID && !deleteTeam {
		next.LeaderID = next.Members[0].ID // members are in join order
		newLeader = &next.LeaderID
	}
	next.Version = t.Version.Next(s.rules.Epoch)

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		return s.db.removeMember(ctx, t.ID, userID, newLeader, deleteTeam)
	}); err != nil {
		return s.reconcile(e, t.ID, err)
	}

	// 5. apply
	s.mu.Lock()
	delete(s.userTeam, userID)
	if deleteTeam {
		delete(s.teams, t.ID)
	}
	s.mu.Unlock()
	if deleteTeam {
		e.gone = true
		// 6. publish
		s.sendNoTeam(noTeamDeleted, userID)
		return nil
	}
	e.cur.Store(&next)
	// 6. publish
	typ := TeamMemberLeft
	if newLeader != nil {
		typ = TeamLeaderChanged
	}
	s.publish(&next, typ)
	s.sendNoTeam(noTeamLeft, userID)
	return nil
}

// SetReady sets a non-leader member's ready flag while the team is in LOBBY.
func (s *Store) SetReady(ctx context.Context, userID uuid.UUID, ready bool) (Team, error) {
	// 1. lock
	e, t, err := s.lockUsersTeam(userID)
	if err != nil {
		return Team{}, err
	}
	defer e.mu.Unlock()

	// 2. validate
	if t.Status != Lobby {
		return Team{}, ErrTeamLocked
	}
	if t.LeaderID == userID {
		return Team{}, ErrLeaderReady
	}

	// 3. build
	next := *t
	next.Members = slices.Clone(t.Members)
	for i := range next.Members {
		if next.Members[i].ID == userID {
			if next.Members[i].IsReady == ready {
				return *t, nil // no change: nothing to persist or publish
			}
			next.Members[i].IsReady = ready
		}
	}
	next.Version = t.Version.Next(s.rules.Epoch)

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		return s.db.updateReady(ctx, t.ID, userID, ready)
	}); err != nil {
		return Team{}, s.reconcile(e, t.ID, err)
	}

	// 5. apply
	e.cur.Store(&next)
	// 6. publish
	s.publish(&next, TeamReadyChanged)
	return next, nil
}

// RefreshProfile updates userID's name and avatar in their team, if they're
// in one, and publishes the team. The user use case has already saved the
// profile (teams reads names and avatars from userprofile at load), so there
// is nothing to persist: steps 4 and 5 collapse into apply. Called after
// every sign-in; a no-op when nothing changed or the user has no team.
func (s *Store) RefreshProfile(userID uuid.UUID, name string, avatarURL *string) {
	// 1. lock
	e, t, err := s.lockUsersTeam(userID)
	if err != nil {
		return // not in a team
	}
	defer e.mu.Unlock()

	// 2. validate: anything to change?
	i := slices.IndexFunc(t.Members, func(m Member) bool { return m.ID == userID })
	m := t.Members[i]
	if m.Name == name && equalPtr(m.AvatarURL, avatarURL) {
		return
	}

	// 3. build
	next := *t
	next.Members = slices.Clone(t.Members)
	next.Members[i].Name, next.Members[i].AvatarURL = name, avatarURL
	next.Version = t.Version.Next(s.rules.Epoch)

	// 5. apply
	e.cur.Store(&next)
	// 6. publish
	s.publish(&next, TeamProfileUpdated)
}

// equalPtr reports whether a and b are both nil or point to equal strings.
func equalPtr(a, b *string) bool {
	return a == b || (a != nil && b != nil && *a == *b)
}

// Start moves the leader's team from LOBBY to IN_GAME once every non-leader
// member is ready. Any size from 1 (the leader alone) to Rules.Size may start.
// From then on nobody can join, leave or change ready; the roster only
// changes when an admin removes a member (RemoveMember). After publishing the
// "team" event it calls Rules.OnStart (if set) with the started team, still
// holding the team's lock; a rejected or failed start doesn't call it.
func (s *Store) Start(ctx context.Context, userID uuid.UUID) (Team, error) {
	// 1. lock
	e, t, err := s.lockUsersTeam(userID)
	if err != nil {
		return Team{}, err
	}
	defer e.mu.Unlock()

	// 2. validate
	if t.LeaderID != userID {
		return Team{}, ErrNotLeader
	}
	if t.Status != Lobby {
		return Team{}, ErrTeamLocked
	}
	for _, m := range t.Members {
		if m.ID != t.LeaderID && !m.IsReady {
			return Team{}, ErrNotAllReady
		}
	}

	// 4. persist, before 3. build: Postgres sets started_at
	var startedAt time.Time
	if err := s.persist(ctx, func(ctx context.Context) (err error) {
		startedAt, err = s.db.updateStarted(ctx, t.ID)
		return err
	}); err != nil {
		return Team{}, s.reconcile(e, t.ID, err)
	}

	// 3. build
	next := *t
	next.Status = InGame
	next.StartedAt = &startedAt
	next.Version = t.Version.Next(s.rules.Epoch)
	// 5. apply
	e.cur.Store(&next)
	// 6. publish: the team, then (through OnStart) anything other use cases
	// show for a started team. Still under the lock, so a member's stream
	// gets "team" before what follows from it.
	s.publish(&next, TeamStarted)
	if s.rules.OnStart != nil {
		s.rules.OnStart(next)
	}
	return next, nil
}

// RemoveMember is the admin's removal of userID from teamID, in any status
// (LOBBY, IN_GAME or FINISHED). If userID leads the team, newLeader must name
// a remaining member, who becomes leader; otherwise newLeader must be nil.
// The team's only member can't be removed (ErrLastMember): this never deletes
// a team. The removed user gets no_team and is free to create or join a team;
// their past scans and casts stay with the team. For a team that has started,
// it calls Rules.OnRosterChange (if set) after publishing, still holding the
// team's lock.
func (s *Store) RemoveMember(ctx context.Context, teamID string, userID uuid.UUID, newLeader *uuid.UUID) (Team, error) {
	// 1. lock
	e, t, err := s.lockTeam(teamID)
	if err != nil {
		return Team{}, err
	}
	defer e.mu.Unlock()

	// 2. validate
	isMember := func(id uuid.UUID) bool {
		return slices.ContainsFunc(t.Members, func(m Member) bool { return m.ID == id })
	}
	if !isMember(userID) {
		return Team{}, ErrNotMember
	}
	if len(t.Members) == 1 {
		return Team{}, ErrLastMember
	}
	switch {
	case t.LeaderID == userID && newLeader == nil:
		return Team{}, ErrNewLeaderRequired
	case t.LeaderID == userID && (*newLeader == userID || !isMember(*newLeader)):
		return Team{}, ErrInvalidNewLeader
	case t.LeaderID != userID && newLeader != nil:
		return Team{}, ErrInvalidNewLeader
	}

	// 3. build
	next := *t
	next.Members = slices.DeleteFunc(slices.Clone(t.Members), func(m Member) bool { return m.ID == userID })
	if newLeader != nil {
		next.LeaderID = *newLeader
	}
	next.Version = t.Version.Next(s.rules.Epoch)

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		return s.db.removeMember(ctx, t.ID, userID, newLeader, false)
	}); err != nil {
		return Team{}, s.reconcile(e, t.ID, err)
	}

	// 5. apply
	s.mu.Lock()
	delete(s.userTeam, userID)
	s.mu.Unlock()
	e.cur.Store(&next)
	// 6. publish: the team to its members, no_team to the removed user, then
	// (through OnRosterChange) whatever other use cases derive from a
	// started team's roster.
	typ := TeamMemberRemoved
	if newLeader != nil {
		typ = TeamLeaderChanged
	}
	s.publish(&next, typ)
	s.sendNoTeam(noTeamRemoved, userID)
	if next.Status != Lobby && s.rules.OnRosterChange != nil {
		s.rules.OnRosterChange(next)
	}
	return next, nil
}
