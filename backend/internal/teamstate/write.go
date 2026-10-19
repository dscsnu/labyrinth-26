package teamstate

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"

	"labyrinth/internal/atlas"
	"labyrinth/internal/teams"
)

// Writes follow the same write-through steps as teams, marked in each
// function:
//
//	1. lock      the team's state entry, so writes to one team are serialised
//	2. validate  against the team (must be IN_GAME), the content and the state
//	3. build     the next State (a copy; the current one is never modified)
//	4. persist   to Postgres via db.go. If this fails, the team's state is
//	             reloaded from Postgres and nothing is applied
//	5. apply     swap in the next State
//	6. publish   send it to the team's live streams as "team_state"
//
// As AGENTS.md §6 allows, the checks that need no lock run first, labelled
// "2. validate" before "1. lock": the content (static) and the team (an
// IN_GAME team's status never changes). Membership and the State are checked
// again under the lock, labelled "2. validate" again: an admin can remove a
// member from a started team (teams.RemoveMember, then RosterChanged, which
// takes the same lock), so a write either runs before the removal is
// visible or is refused with ErrNotInTeam after it.
//
// The game flow. A member selects a shape, which always purges the team's
// scan stack. There's no time limit, and any member may scan any number of
// the pattern's waypoints. A scan is rejected without being pushed if:
//
//   - no shape is selected
//   - the attempt is poisoned (a wrong scan is in the stack): select again
//   - its waypoint is already in the stack (by anyone)
//
// Otherwise it's pushed. If the stack plus this waypoint can no longer become
// a pattern of the selected shape, the scan is pushed marked wrong and the
// player is told (OutcomeWrongScan): the attempt is poisoned until someone
// selects again, which purges it. Once the stack is exactly a pattern of the
// selected shape, the pattern completes (a cast, logged in team_cast) and the
// stack and selection are purged.

var (
	// ErrNotInGame means the team hasn't started (or has finished).
	ErrNotInGame = errors.New("team is not in a game")
	// ErrUnknownWaypoint means no waypoint has that ID.
	ErrUnknownWaypoint = errors.New("unknown waypoint")
	// ErrUnknownShape means no shape has that ID.
	ErrUnknownShape = errors.New("unknown shape")
	// ErrNoShapeSelected means the team must select a shape before scanning.
	ErrNoShapeSelected = errors.New("select a shape before scanning")
	// ErrWaypointAlreadyScanned means this waypoint is already in the stack.
	ErrWaypointAlreadyScanned = errors.New("this waypoint is already scanned")
	// ErrSelectionPoisoned means a wrong scan is in the stack: the attempt
	// can't cast, and the team must select the shape again to start over.
	ErrSelectionPoisoned = errors.New("a scan didn't fit any pattern of this shape; select the shape again to start over")
)

// Scan outcomes, reported in ScanResult.Outcome.
const (
	// OutcomeScanAdded means the scan was pushed onto the stack, but no
	// pattern is complete yet.
	OutcomeScanAdded = "scan_added"
	// OutcomeWrongScan means the scan was pushed but doesn't fit any pattern
	// of the selected shape (with the scans before it): the attempt is
	// poisoned until the team selects again.
	OutcomeWrongScan = "wrong_scan"
	// OutcomePatternCompleted means the scan completed a pattern of the
	// selected shape; the selection and stack are purged.
	OutcomePatternCompleted = "pattern_completed"
)

// ScanResult is what an accepted scan did.
type ScanResult struct {
	// Outcome is one of the Outcome constants.
	Outcome string `json:"outcome"`
	// Pattern is the completed pattern's waypoint IDs, when Outcome is
	// OutcomePatternCompleted.
	Pattern []string `json:"pattern,omitempty"`
	// AlreadyCompleted reports that the completed pattern had been completed
	// before, so the team's progress didn't grow.
	AlreadyCompleted bool `json:"already_completed,omitempty"`
	// State is the team's state after the scan.
	State View `json:"state"`
}

// inGameTeam returns userID's team if it's IN_GAME. Read without the team's
// lock: once IN_GAME, a team's status never changes. Its roster can shrink
// (an admin's teams.RemoveMember), so writers check isMember again under the
// entry's lock.
func (s *Store) inGameTeam(userID uuid.UUID) (teams.Team, error) {
	t, err := s.teams.ForUser(userID)
	if err != nil {
		return t, err
	}
	if t.Status != teams.InGame {
		return t, ErrNotInGame
	}
	return t, nil
}

// Select makes shapeID the team's selected shape and purges its stack. Every
// selection purges, including reselecting the same shape, so scans from an
// earlier attempt never count toward a new one.
func (s *Store) Select(ctx context.Context, userID uuid.UUID, shapeID string) (View, error) {
	// 2. validate, before 1. lock: content and team need no lock
	shape, ok := s.atlas.ShapeIndex(shapeID)
	if !ok {
		return View{}, ErrUnknownShape
	}
	t, err := s.inGameTeam(userID)
	if err != nil {
		return View{}, err
	}

	// 1. lock, then 2. validate membership again under it: an admin may
	// have removed the user since the lookup.
	e := s.lockTeam(t.ID)
	defer e.mu.Unlock()
	if !s.isMember(t.ID, userID) {
		return View{}, ErrNotInTeam
	}
	cur := e.cur.Load()

	// 3. build
	now := s.now()
	next := *cur
	next.Selection = Selection{Active: true, Shape: shape, By: userID, At: now}
	next.Stack = NewStack(s.rules.MaxPatternPoints)
	next.Version = cur.Version.Next(s.rules.Epoch)

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		return s.db.upsertSelection(ctx, t.ID, shapeID, userID, now)
	}); err != nil {
		return View{}, s.reconcile(e, t.ID, err)
	}

	// 5. apply
	e.cur.Store(&next)
	// 6. publish
	s.publish(e, t.ID, &next, ShapeSelected)
	return s.view(t.ID, next), nil
}

// ClearSelection removes the team's selected shape, which also empties its
// stack. Clearing when nothing is selected is a no-op.
func (s *Store) ClearSelection(ctx context.Context, userID uuid.UUID) (View, error) {
	// 2. validate, before 1. lock: the team needs no lock
	t, err := s.inGameTeam(userID)
	if err != nil {
		return View{}, err
	}

	// 1. lock, then 2. validate membership again under it: an admin may
	// have removed the user since the lookup.
	e := s.lockTeam(t.ID)
	defer e.mu.Unlock()
	if !s.isMember(t.ID, userID) {
		return View{}, ErrNotInTeam
	}
	cur := e.cur.Load()

	// 2. validate (state)
	if !cur.Selection.Active {
		return s.view(t.ID, *cur), nil // nothing to clear
	}

	// 3. build
	next := *cur
	next.Selection = Selection{}
	next.Stack = NewStack(s.rules.MaxPatternPoints)
	next.Version = cur.Version.Next(s.rules.Epoch)

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		return s.db.deleteSelection(ctx, t.ID)
	}); err != nil {
		return View{}, s.reconcile(e, t.ID, err)
	}

	// 5. apply
	e.cur.Store(&next)
	// 6. publish
	s.publish(e, t.ID, &next, SelectionCleared)
	return s.view(t.ID, next), nil
}

// Scan records that userID scanned waypointID (4 digits) toward the team's
// selected shape, and completes the pattern once the stack is exactly one.
// See the rules above. A rejected scan returns an error and changes nothing;
// a wrong scan is still pushed, marked wrong, and poisons the selection.
func (s *Store) Scan(ctx context.Context, userID uuid.UUID, waypointID string) (ScanResult, error) {
	// 2. validate, before 1. lock: content and team need no lock
	w, ok := s.atlas.WaypointIndex(waypointID)
	if !ok {
		return ScanResult{}, ErrUnknownWaypoint
	}
	t, err := s.inGameTeam(userID)
	if err != nil {
		return ScanResult{}, err
	}

	// 1. lock, then 2. validate membership again under it: an admin may
	// have removed the user since the lookup.
	e := s.lockTeam(t.ID)
	defer e.mu.Unlock()
	if !s.isMember(t.ID, userID) {
		return ScanResult{}, ErrNotInTeam
	}
	cur := e.cur.Load()
	now := s.now()

	// 2. validate (state)
	if !cur.Selection.Active {
		return ScanResult{}, ErrNoShapeSelected
	}
	if cur.Poisoned() {
		return ScanResult{}, ErrSelectionPoisoned
	}
	var set atlas.WaypointSet
	for i := range cur.Stack.Len() {
		sc := cur.Stack.At(i)
		if sc.Waypoint == w {
			return ScanResult{}, ErrWaypointAlreadyScanned
		}
		set = set.With(sc.Waypoint)
	}
	set = set.With(w)
	// A scan that doesn't fit isn't rejected: it's pushed, marked wrong, the
	// player is told, and the attempt is poisoned until the team reselects.
	wrong := !s.atlas.FitsShape(cur.Selection.Shape, set)

	// 3. build
	next := *cur
	next.Waypoints = cur.Waypoints.With(w)
	next.Stack = cur.Stack.Push(StackScan{Waypoint: w, UserID: userID, At: now, Wrong: wrong})
	next.Version = cur.Version.Next(s.rules.Epoch)
	res := ScanResult{Outcome: OutcomeScanAdded}
	typ := ScanAdded
	if wrong {
		res.Outcome, typ = OutcomeWrongScan, WrongScan
	}
	// A wrong scan can't complete: the set fits no pattern of the shape.
	completed, isComplete := s.atlas.PatternFor(set)
	isComplete = !wrong && isComplete && s.atlas.Pattern(completed).Shape == cur.Selection.Shape
	if isComplete {
		res.Outcome, typ = OutcomePatternCompleted, SpellCast
		res.Pattern = s.atlas.WaypointIDs(set)
		res.AlreadyCompleted = cur.Patterns.Has(completed)
		next.Patterns = cur.Patterns.With(completed)
		next.Shapes = cur.Shapes.With(cur.Selection.Shape)
		next.Selection = Selection{}
		next.Stack = NewStack(s.rules.MaxPatternPoints)
		// Copy-on-write: never append to the current snapshot's slice.
		next.Casts = append(slices.Clip(cur.Casts), Cast{Pattern: completed, By: userID, At: now, AlreadyCompleted: res.AlreadyCompleted})
	}

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		if isComplete {
			return s.db.insertCompletingScan(ctx, t.ID, waypointID, userID, now, s.atlas.Pattern(completed).ID, wrong, res.AlreadyCompleted)
		}
		return s.db.insertScan(ctx, t.ID, waypointID, userID, now, wrong)
	}); err != nil {
		return ScanResult{}, s.reconcile(e, t.ID, err)
	}

	// 5. apply
	e.cur.Store(&next)
	// 6. publish
	s.publish(e, t.ID, &next, typ)
	res.State = s.view(t.ID, next)
	return res, nil
}

// isMember reports whether userID is in teamID's current roster, read from
// teams (lock-free). Called with the team's entry locked.
func (s *Store) isMember(teamID string, userID uuid.UUID) bool {
	t, ok := s.teams.ByID(teamID)
	return ok && slices.ContainsFunc(t.Members, func(m teams.Member) bool { return m.ID == userID })
}

// RosterChanged is teams.Rules.OnRosterChange: an admin removed a member from
// teamID after it started. It drops the entry's cached roster, so the next
// team_state goes to the current members only. Memory-only (AGENTS.md §6):
// the roster is teams' data, already saved; and no team_state is published,
// since the state itself didn't change. Taking the entry's lock also orders
// the removal against in-flight writes (isMember).
func (s *Store) RosterChanged(teamID string) {
	e := s.lockTeam(teamID)
	defer e.mu.Unlock()
	e.members = nil
}

// PublishTeam sends teamID's current state to its members as "team_state",
// changing nothing. teams calls it (through teams.Rules.OnStart, wired in
// server.Build) right after a team starts, so every member gets the new
// team's (empty) state after its "team" event without reconnecting. It
// publishes nothing if the team doesn't exist or isn't IN_GAME.
//
// Not a write: nothing is built, persisted or applied, so it keeps only steps
// 1, 2 and 6. It takes the team's state lock so its event is ordered with
// the team's writes. The state entry it locks is created empty if the team
// had none: the same empty State ForTeam reports for a team without an entry,
// so reads (and a restart, which creates no entry) see no difference.
func (s *Store) PublishTeam(teamID string) {
	// 2. validate, before 1. lock: an IN_GAME team's status never changes
	t, ok := s.teams.ByID(teamID)
	if !ok || t.Status != teams.InGame {
		return
	}

	// 1. lock
	e := s.lockTeam(teamID)
	defer e.mu.Unlock()

	// 6. publish
	s.publish(e, teamID, e.cur.Load(), GameStarted)
}
