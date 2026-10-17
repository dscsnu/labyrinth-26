package teamstate

import (
	"context"
	"errors"

	"github.com/google/uuid"

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
// IN_GAME team's status and roster are frozen). Checks against the State run
// under the lock, labelled "2. validate" again.
//
// The game flow. A member selects a shape, which always purges the team's
// scan stack. Each scan is checked against the stack's live scans (stale ones,
// older than window + grace, are ignored and dropped), and is rejected
// without being pushed if:
//
//   - its waypoint is already in the stack (by anyone, the scanner included)
//   - its member already has a scan in the stack
//   - the stack plus this waypoint isn't part of any pattern of the shape
//
// So the stack only ever holds one scan per point of a pattern still in
// reach, each by a different member. Once it is exactly a pattern of the
// selected shape, the pattern completes and the stack and selection are
// purged.

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
	// ErrMemberAlreadyScanned means this member already has a scan in the
	// stack; a teammate must scan the next waypoint.
	ErrMemberAlreadyScanned = errors.New("you already scanned a waypoint; a teammate must scan the next one")
	// ErrNotInPattern means the waypoint, with the ones already scanned, isn't
	// part of any pattern of the selected shape.
	ErrNotInPattern = errors.New("this waypoint isn't part of the pattern")
)

// Scan outcomes, reported in ScanResult.Outcome.
const (
	// OutcomeScanAdded means the scan was pushed onto the stack, but no
	// pattern is complete yet.
	OutcomeScanAdded = "scan_added"
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
// lock: once IN_GAME, a team's status and roster never change.
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
	shape, ok := s.content.ShapeIndex(shapeID)
	if !ok {
		return View{}, ErrUnknownShape
	}
	t, err := s.inGameTeam(userID)
	if err != nil {
		return View{}, err
	}

	// 1. lock
	e := s.lockTeam(t.ID)
	defer e.mu.Unlock()
	cur := e.cur.Load()

	// 3. build
	now := s.now()
	next := *cur
	next.Selection = Selection{Active: true, Shape: shape, By: userID, At: now}
	next.Stack = newStack(s.rules.MaxPatternPoints)

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		return s.db.upsertSelection(ctx, t.ID, shapeID, userID, now)
	}); err != nil {
		return View{}, s.reconcile(e, t.ID, err)
	}

	// 5. apply
	e.cur.Store(&next)
	// 6. publish
	s.publish(t.ID, &next)
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

	// 1. lock
	e := s.lockTeam(t.ID)
	defer e.mu.Unlock()
	cur := e.cur.Load()

	// 2. validate (state)
	if !cur.Selection.Active {
		return s.view(t.ID, *cur), nil // nothing to clear
	}

	// 3. build
	next := *cur
	next.Selection = Selection{}
	next.Stack = newStack(s.rules.MaxPatternPoints)

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		return s.db.deleteSelection(ctx, t.ID)
	}); err != nil {
		return View{}, s.reconcile(e, t.ID, err)
	}

	// 5. apply
	e.cur.Store(&next)
	// 6. publish
	s.publish(t.ID, &next)
	return s.view(t.ID, next), nil
}

// Scan records that userID scanned waypointID (4 digits) toward the team's
// selected shape, and completes the pattern once the stack is exactly one.
// See the rules above. A rejected scan returns an error and changes nothing.
func (s *Store) Scan(ctx context.Context, userID uuid.UUID, waypointID string) (ScanResult, error) {
	// 2. validate, before 1. lock: content and team need no lock
	w, ok := s.content.WaypointIndex(waypointID)
	if !ok {
		return ScanResult{}, ErrUnknownWaypoint
	}
	t, err := s.inGameTeam(userID)
	if err != nil {
		return ScanResult{}, err
	}

	// 1. lock
	e := s.lockTeam(t.ID)
	defer e.mu.Unlock()
	cur := e.cur.Load()
	now := s.now()

	// 2. validate (state), against the stack's live scans
	if !cur.Selection.Active {
		return ScanResult{}, ErrNoShapeSelected
	}
	live := cur.Stack.Live(now, s.rules.limit())
	var set WaypointSet
	for _, sc := range live {
		if sc.Waypoint == w {
			return ScanResult{}, ErrWaypointAlreadyScanned
		}
		if sc.UserID == userID {
			return ScanResult{}, ErrMemberAlreadyScanned
		}
		set = set.With(sc.Waypoint)
	}
	set = set.With(w)
	if !s.content.FitsShape(cur.Selection.Shape, set) {
		return ScanResult{}, ErrNotInPattern
	}

	// 3. build: drop stale scans (keep the live ones), push this one. Every
	// live scan is at most the limit old and this one is now, so bottom to
	// top spans at most the limit.
	stack := newStack(s.rules.MaxPatternPoints)
	for _, sc := range live {
		stack = stack.Push(sc)
	}
	stack = stack.Push(StackScan{Waypoint: w, UserID: userID, At: now})

	next := *cur
	next.Waypoints = cur.Waypoints.With(w)
	next.Stack = stack
	res := ScanResult{Outcome: OutcomeScanAdded}
	completed, isComplete := s.content.PatternFor(set)
	isComplete = isComplete && s.content.Patterns[completed].Shape == cur.Selection.Shape
	if isComplete {
		res.Outcome = OutcomePatternCompleted
		res.Pattern = s.content.WaypointIDs(set)
		res.AlreadyCompleted = cur.Patterns.Has(completed)
		next.Patterns = cur.Patterns.With(completed)
		next.Shapes = cur.Shapes.With(cur.Selection.Shape)
		next.Selection = Selection{}
		next.Stack = newStack(s.rules.MaxPatternPoints)
	}

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		if isComplete {
			return s.db.insertCompletingScan(ctx, t.ID, waypointID, userID, now, s.content.Patterns[completed].dbID)
		}
		return s.db.insertScan(ctx, t.ID, waypointID, userID, now)
	}); err != nil {
		return ScanResult{}, s.reconcile(e, t.ID, err)
	}

	// 5. apply
	e.cur.Store(&next)
	// 6. publish
	s.publish(t.ID, &next)
	res.State = s.view(t.ID, next)
	return res, nil
}
