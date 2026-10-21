package teamstate

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/atlas"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/qr"
	"labyrinth/internal/qte"
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
// The game flow. The leader selects a shape, which always purges the team's
// scan stack. There's no time limit, and any member may scan any number of
// the pattern's waypoints. A scan is rejected without being pushed if:
//
//   - no shape is selected
//   - the attempt is poisoned (a wrong scan is in the stack): select again
//   - its waypoint is already in the stack (by anyone)
//
// Otherwise it's pushed. If the stack plus this waypoint can no longer become
// a pattern of the selected shape, the scan is pushed marked wrong and the
// player is told (OutcomeWrongScan): the attempt is poisoned until the leader
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
	// ErrNotLeader is the teams use case's: only the leader selects a shape
	// or clears the selection.
	ErrNotLeader = teams.ErrNotLeader
	// ErrShapeLocked means every pattern of the shape is bigger than the
	// team has unlocked (game.locked_pattern_points, until it has defeated
	// game.unlock_after_bosses bosses).
	ErrShapeLocked = errors.New("this shape's patterns are locked until your team defeats more bosses")
	// ErrShapeOnCooldown means every pattern of the shape the team has
	// unlocked is of the spell class of its last cast: it must cast a spell
	// of another class first.
	ErrShapeOnCooldown = errors.New("every spell of this shape is on cooldown: cast a spell of another class first")
	// ErrWaypointOnCooldown means the waypoint was in one of the team's
	// casts less than game.qr_cooldown ago: the team can't scan it yet.
	ErrWaypointOnCooldown = errors.New("your team cast a spell with this QR code recently: it opens again soon")
	// ErrKnockedOut means the team's health is 0 (boss attacks): it can't
	// select or scan until an admin revives it.
	ErrKnockedOut = errors.New("your team is knocked out; an admin must revive it")
	// ErrTeamNotFound is the teams use case's: no team has that ID (Revive).
	ErrTeamNotFound = teams.ErrTeamNotFound
	// ErrSelectionPoisoned means a wrong scan is in the stack: the attempt
	// can't cast, and the team must select the shape again to start over.
	ErrSelectionPoisoned = errors.New("a scan didn't fit any pattern of this shape; select the shape again to start over")
	// ErrInvalidQR means the scan's token is malformed or its signature
	// doesn't check out, so the scan is refused before it names a waypoint.
	ErrInvalidQR = errors.New("invalid qr token")
	// ErrExpiredQR means the scan's token is signed and well-formed but
	// outside its validity window, so the scan is refused.
	ErrExpiredQR = errors.New("qr token expired")
	// ErrUnknownPowerup means no powerup kind has that name.
	ErrUnknownPowerup = errors.New("unknown powerup")
	// ErrPowerupNotHeld means the team doesn't hold a powerup of that kind
	// (never earned, or already used: a resent activation lands here).
	ErrPowerupNotHeld = errors.New("your team doesn't hold this powerup")
	// ErrPowerupActive means a powerup of that kind is still active (an
	// armed damage multiplier, a running short QR cooldown): it can't be
	// activated again until it ends.
	ErrPowerupActive = errors.New("this powerup is already active")
	// ErrFullHealth means the team's health is at game.health: a heart
	// would do nothing.
	ErrFullHealth = errors.New("your team is at full health")
	// ErrUnknownPattern means the waypoints of an instant cast make up no
	// pattern.
	ErrUnknownPattern = errors.New("those waypoints make no pattern")
	// ErrPatternLocked means an instant cast's pattern is bigger than the
	// team has unlocked (game.locked_pattern_points, until it has defeated
	// game.unlock_after_bosses bosses).
	ErrPatternLocked = errors.New("this pattern is locked until your team defeats more bosses")
	// ErrUnknownQTE is the qte use case's: no QTE has that ID (or it's a
	// draft, which players can't see).
	ErrUnknownQTE = qte.ErrUnknownQTE
	// ErrQTEClosed is the qte use case's: the QTE's window is over.
	ErrQTEClosed = qte.ErrQTEClosed
	// ErrInvalidOption means the answer names no option of the quiz.
	ErrInvalidOption = errors.New("that isn't one of the options")
	// ErrQTEAlreadyAnswered means someone on the team has answered this QTE:
	// one answer per team (a resend lands here).
	ErrQTEAlreadyAnswered = errors.New("your team has already answered")
	// errNeedsPattern means Activate was asked for an instant_cast, which
	// needs a pattern: InstantCast casts it. The API never does this.
	errNeedsPattern = errors.New("an instant_cast is activated by InstantCast, with a pattern")
)

// GrantResult is what GrantPowerup did: whether the team got the powerup
// (false: it already held one, so this one is wasted) and its state.
type GrantResult struct {
	// Granted reports that the team now holds the powerup because of this
	// grant; false means it already held one of that kind.
	Granted bool `json:"granted"`
	// State is the team's state after the grant.
	State View `json:"state"`
}

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
	// Any boss attack that fell due is applied first (its own change), so
	// this write sees the team's real health.
	cur := s.catchUp(e, t.ID)

	// 2. validate (state)
	if !s.isLeader(t.ID, userID) {
		return View{}, ErrNotLeader
	}
	if cur.Boss.KnockedOut() {
		return View{}, ErrKnockedOut
	}
	if s.shapeLocked(shape, s.unlockedPoints(*cur)) {
		return View{}, ErrShapeLocked
	}
	if s.shapeOnCooldown(shape, s.unlockedPoints(*cur), s.cooldown(*cur)) {
		return View{}, ErrShapeOnCooldown
	}

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
	// Any boss attack that fell due is applied first (its own change).
	// Clearing is allowed even when knocked out: it only gives up.
	cur := s.catchUp(e, t.ID)

	// 2. validate (state)
	if !s.isLeader(t.ID, userID) {
		return View{}, ErrNotLeader
	}

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

// Scan records that userID scanned the waypoint a token names toward the
// team's selected shape, and completes the pattern once the stack is exactly
// one. The token is verified first (signature and freshness): an invalid or
// expired token is refused before it names a waypoint. See the rules above. A
// rejected scan returns an error and changes nothing; a wrong scan is still
// pushed, marked wrong, and poisons the selection.
func (s *Store) Scan(ctx context.Context, userID uuid.UUID, token string) (ScanResult, error) {
	// 2. validate, before 1. lock: verifying the token, then resolving the
	// waypoint and team, all need no lock.
	waypointID, err := s.qr.Verify(token)
	if err != nil {
		switch {
		case errors.Is(err, qr.ErrInvalid):
			return ScanResult{}, ErrInvalidQR
		case errors.Is(err, qr.ErrExpired):
			return ScanResult{}, ErrExpiredQR
		default:
			return ScanResult{}, err
		}
	}
	w, ok := s.atlas.WaypointIndex(waypointID)
	if !ok {
		return ScanResult{}, ErrUnknownWaypoint
	}
	t, err := s.inGameTeam(userID)
	if err != nil {
		return ScanResult{}, err
	}
	// A cast that defeats the last boss finishes the team, once the lock
	// below is released (Rules.OnFinished: no lock held while teams works).
	var finished bool
	defer func() {
		if finished && s.rules.OnFinished != nil {
			s.rules.OnFinished(t.ID)
		}
	}()

	// 1. lock, then 2. validate membership again under it: an admin may
	// have removed the user since the lookup.
	e := s.lockTeam(t.ID)
	defer e.mu.Unlock()
	if !s.isMember(t.ID, userID) {
		return ScanResult{}, ErrNotInTeam
	}
	// Any boss attack that fell due is applied first (its own change), so
	// this scan sees the team's real health.
	cur := s.catchUp(e, t.ID)
	now := s.now()

	// 2. validate (state)
	if cur.Boss.KnockedOut() {
		return ScanResult{}, ErrKnockedOut
	}
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
	// The waypoint was in one of the team's casts less than
	// game.qr_cooldown ago: refused, nothing written or published.
	if _, ok := cur.qrCooldownUntil(w, now); ok {
		return ScanResult{}, ErrWaypointOnCooldown
	}
	set = set.With(w)
	// A scan that doesn't fit isn't rejected: it's pushed, marked wrong, the
	// player is told, and the attempt is poisoned until the team reselects.
	// Patterns the team can't cast now don't count: those it hasn't unlocked
	// and those of its cooldown class (its last cast's). A scan that could
	// only grow into one of them is wrong too.
	wrong := !s.atlas.FitsShapeCastable(cur.Selection.Shape, set, s.unlockedPoints(*cur), s.cooldown(*cur))

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
	var cast Cast
	if isComplete {
		res.Outcome = OutcomePatternCompleted
		res.Pattern = s.atlas.WaypointIDs(set)
		cast, typ, finished = s.applyCast(cur, &next, completed, userID, now, false)
		res.AlreadyCompleted = cast.AlreadyCompleted
	}

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		if isComplete {
			return s.db.insertCompletingScan(ctx, t.ID, waypointID, userID, now, s.atlas.Pattern(completed).ID, cast, next.Boss)
		}
		return s.db.insertScan(ctx, t.ID, waypointID, userID, now, wrong)
	}); err != nil {
		return ScanResult{}, s.reconcile(e, t.ID, err)
	}

	// 5. apply
	e.cur.Store(&next)
	// 6. publish, and re-arm the timer: a defeat starts the next boss's
	// clock, and the cast's QR cooldowns end at their own time
	s.publish(e, t.ID, &next, typ)
	if isComplete {
		s.schedule(e, t.ID, &next)
	}
	res.State = s.view(t.ID, next)
	return res, nil
}

// applyCast casts pattern p for by at now onto next, a copy of cur, as part
// of a write's build step: the pattern and its shape are completed, the
// selection and stack purged, its damage dealt to the current boss (deal),
// the cast appended to the log (copy-on-write: never to cur's slice) and its
// waypoints put on QR cooldown. An armed damage_multiplier multiplies its
// damage and is used up; a running short_qr_cooldown shortens its QR
// cooldown. instant marks a cast by an instant_cast powerup. It returns the
// cast, the event type (SpellCast, or BossDefeated) and whether it defeated
// the last boss (the caller then calls Rules.OnFinished once unlocked).
func (s *Store) applyCast(cur, next *State, p int, by uuid.UUID, now time.Time, instant bool) (Cast, live.EventType, bool) {
	pat := s.atlas.Pattern(p)
	next.Patterns = cur.Patterns.With(p)
	next.Shapes = cur.Shapes.With(pat.Shape)
	next.Selection = Selection{}
	next.Stack = NewStack(s.rules.MaxPatternPoints)
	c := Cast{Pattern: p, By: by, At: now, AlreadyCompleted: cur.Patterns.Has(p), Instant: instant,
		ShortQR: now.Before(cur.Powerups.ShortQRUntil)}
	factor := 1.0
	if cur.Powerups.MultiplierArmed {
		factor, c.Boosted = s.rules.PowerupMultiplier, true
		next.Powerups.MultiplierArmed = false // the log grows past its activation length
	}
	next.Boss, c.Dealt, c.Boss, c.DefeatedBoss = deal(cur.Boss, pat.Spell.Damage, factor, now, s.bosses)
	typ := SpellCast
	if c.DefeatedBoss {
		typ = BossDefeated
	}
	next.Casts = append(slices.Clip(cur.Casts), c)
	next.QRCooldowns = s.qrCooldowns(next.Casts, next.CooldownsReset, now)
	return c, typ, c.DefeatedBoss && next.Boss.Defeated >= s.bosses.Count()
}

// isMember reports whether userID is in teamID's current roster, read from
// teams (lock-free). Called with the team's entry locked.
func (s *Store) isMember(teamID string, userID uuid.UUID) bool {
	t, ok := s.teams.ByID(teamID)
	return ok && slices.ContainsFunc(t.Members, func(m teams.Member) bool { return m.ID == userID })
}

// isLeader reports whether userID leads teamID now, read from teams
// (lock-free). Called with the team's entry locked: an admin removing the
// leader names a new one.
func (s *Store) isLeader(teamID string, userID uuid.UUID) bool {
	t, ok := s.teams.ByID(teamID)
	return ok && t.LeaderID == userID
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

	// 6. publish, and start the first boss's attack clock
	s.publish(e, teamID, e.cur.Load(), GameStarted)
	s.schedule(e, teamID, e.cur.Load())
}

// catchUp applies whatever has fallen due on teamID's state by itself, and
// returns the current state: boss attacks, QR cooldowns that have ended, and
// the end of a short_qr_cooldown powerup's window.
// The caller holds e.mu. Every write calls it before validating, and so does
// the timer, so a change is never lost to a late timer or a write that got
// the lock first.
//
// All are memory-only changes (AGENTS.md §6): health is derived from the
// stored phase (PhaseStart, PhaseHealth) and the clock, QR cooldowns from the
// cast log and the clock, and the window from team_powerup.activated_at and
// the clock, so there's nothing to persist, and Load reproduces them
// exactly. It still bumps the version and publishes one event, the first
// that applies of: knocked_out (an attack brought health to 0),
// boss_attacked, powerup_ended, qr_cooldown_ended.
func (s *Store) catchUp(e *entry, teamID string) *State {
	cur := e.cur.Load()
	now := s.now()
	p := cur.Boss.at(now, s.bosses)
	attacked := p.Attacks != cur.Boss.Attacks || p.Health != cur.Boss.Health
	cooldowns := cur.QRCooldowns
	ended := len(cooldowns) > 0 && !now.Before(cooldowns[0].Until) // soonest first
	window := cur.Powerups.ShortQRUntil
	windowEnded := !window.IsZero() && !now.Before(window)
	if !attacked && !ended && !windowEnded {
		return cur // nothing fell due
	}

	// 3. build
	next := *cur
	next.Boss = p
	if ended {
		next.QRCooldowns = s.qrCooldowns(cur.Casts, cur.CooldownsReset, now)
	}
	if windowEnded {
		next.Powerups.ShortQRUntil = time.Time{}
	}
	next.Version = cur.Version.Next(s.rules.Epoch)
	// 4. persist: none, memory-only (see above)
	// 5. apply
	e.cur.Store(&next)
	// 6. publish
	typ := QRCooldownEnded
	switch {
	case p.KnockedOut() && attacked:
		typ = KnockedOut
	case attacked:
		typ = BossAttacked
	case windowEnded:
		typ = PowerupEnded
	}
	s.publish(e, teamID, &next, typ)
	return &next
}

// due is the timer's callback for teamID: it applies whatever has fallen due
// (catchUp) and arms the timer for the next. Run by the Clock in its own
// goroutine; does nothing once the store is closed.
func (s *Store) due(teamID string) {
	// 1. lock: the existing entry only. One TeamDeleted removed is never
	// recreated (it's closed, or gone from the index).
	s.mu.RLock()
	e := s.states[teamID]
	s.mu.RUnlock()
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	s.schedule(e, teamID, s.catchUp(e, teamID))
}

// Revive is the admin's revive of teamID: health back to game.health and the
// current boss's attack clock restarted from now (its attacks so far no
// longer count). Allowed whether or not the team is knocked out, so it also
// heals. ErrTeamNotFound if there's no such team, ErrNotInGame unless it's
// IN_GAME.
func (s *Store) Revive(ctx context.Context, teamID string) (View, error) {
	// 2. validate, before 1. lock: an IN_GAME team's status never changes
	t, ok := s.teams.ByID(teamID)
	if !ok {
		return View{}, ErrTeamNotFound
	}
	if t.Status != teams.InGame {
		return View{}, ErrNotInGame
	}

	// 1. lock
	e := s.lockTeam(teamID)
	defer e.mu.Unlock()
	cur := s.catchUp(e, teamID)

	// 3. build: a new phase of the same boss, at full health
	now := s.now()
	next := *cur
	next.Boss.PhaseStart, next.Boss.PhaseHealth = now, s.rules.Health
	next.Boss.Attacks, next.Boss.Health = 0, s.rules.Health
	next.Version = cur.Version.Next(s.rules.Epoch)

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		return s.db.upsertBossPhase(ctx, teamID, next.Boss)
	}); err != nil {
		return View{}, s.reconcile(e, teamID, err)
	}

	// 5. apply
	e.cur.Store(&next)
	// 6. publish, and restart the attack clock
	s.publish(e, teamID, &next, Revived)
	s.schedule(e, teamID, &next)
	return s.view(teamID, next), nil
}

// ResetCooldowns is the admin's reset of teamID's cooldowns: no cast so far
// puts its spell class or its waypoints on cooldown any more
// (State.CooldownsReset, the log's length now). Every later cast starts its
// cooldowns as usual, even one in the same instant.
// ErrTeamNotFound if there's no such team, ErrNotInGame unless it's IN_GAME.
func (s *Store) ResetCooldowns(ctx context.Context, teamID string) (View, error) {
	// 2. validate, before 1. lock: an IN_GAME team's status never goes back
	t, ok := s.teams.ByID(teamID)
	if !ok {
		return View{}, ErrTeamNotFound
	}
	if t.Status != teams.InGame {
		return View{}, ErrNotInGame
	}

	// 1. lock
	e := s.lockTeam(teamID)
	defer e.mu.Unlock()
	cur := s.catchUp(e, teamID)

	// 3. build
	next := *cur
	next.CooldownsReset = len(cur.Casts)
	next.QRCooldowns = nil
	next.Version = cur.Version.Next(s.rules.Epoch)

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		return s.db.upsertCooldownsReset(ctx, teamID, next.CooldownsReset)
	}); err != nil {
		return View{}, s.reconcile(e, teamID, err)
	}

	// 5. apply
	e.cur.Store(&next)
	// 6. publish, and re-arm the timer: no QR cooldown expiry is due now
	s.publish(e, teamID, &next, CooldownsReset)
	s.schedule(e, teamID, &next)
	return s.view(teamID, next), nil
}

// TeamDeleted is teams.Rules.OnDelete: an admin deleted teamID, whose rows
// Postgres has already removed (ON DELETE CASCADE). It stops the team's
// timer and drops its entry. Memory-only (AGENTS.md §6): the data is gone
// from Postgres, which is what Load would rebuild. A write already holding
// the entry finishes first; one after finds no team (not_in_team).
//
// The entry leaves the index first and is then closed under its own lock,
// never holding both (Close takes them the other way round): a timer that
// fires after either step finds nothing to do (due).
func (s *Store) TeamDeleted(teamID string) {
	s.mu.Lock()
	e := s.states[teamID]
	delete(s.states, teamID)
	s.mu.Unlock()
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopTimer != nil {
		e.stopTimer()
		e.stopTimer = nil
	}
	e.closed = true
}

// GrantPowerup gives teamID a powerup of kind: what an admin does now, and a
// quiz will do. A team holds at most one of each kind, so if it already holds
// one the grant is wasted: Granted false, and nothing written or published.
// ErrUnknownPowerup for an unknown kind, ErrTeamNotFound if there's no such
// team, ErrNotInGame unless it's IN_GAME. A knocked-out team can be granted
// one.
func (s *Store) GrantPowerup(ctx context.Context, teamID string, kind Powerup) (GrantResult, error) {
	// 2. validate, before 1. lock: an IN_GAME team's status never goes back
	if kind >= powerupKinds {
		return GrantResult{}, ErrUnknownPowerup
	}
	t, ok := s.teams.ByID(teamID)
	if !ok {
		return GrantResult{}, ErrTeamNotFound
	}
	if t.Status != teams.InGame {
		return GrantResult{}, ErrNotInGame
	}

	// 1. lock
	e := s.lockTeam(teamID)
	defer e.mu.Unlock()
	cur := s.catchUp(e, teamID)

	// 2. validate (state): already held is a no-op, not an error
	if cur.Powerups.Held.Has(kind) {
		return GrantResult{State: s.view(teamID, *cur)}, nil
	}

	// 3. build
	next := *cur
	next.Powerups.Held = cur.Powerups.Held.With(kind)
	next.Version = cur.Version.Next(s.rules.Epoch)

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		return s.db.upsertPowerupHeld(ctx, teamID, kind.ID())
	}); err != nil {
		return GrantResult{}, s.reconcile(e, teamID, err)
	}

	// 5. apply
	e.cur.Store(&next)
	// 6. publish
	s.publish(e, teamID, &next, PowerupGranted)
	return GrantResult{Granted: true, State: s.view(teamID, next)}, nil
}

// Activate uses userID's team's held powerup of kind: a damage_multiplier
// arms the next cast, a heart gives back Rules.PowerupHeart health (never
// above Rules.Health; the attack clock doesn't move), a short_qr_cooldown
// starts its Rules.ShortQRWindow. Only the leader activates. Refused, in
// order: ErrUnknownPowerup, ErrNotInTeam, ErrNotInGame, ErrNotLeader,
// ErrKnockedOut, ErrPowerupNotHeld, ErrPowerupActive (the same kind still
// running), ErrFullHealth (a heart at full health). An instant_cast is
// InstantCast's.
func (s *Store) Activate(ctx context.Context, userID uuid.UUID, kind Powerup) (View, error) {
	// 2. validate, before 1. lock: the kind and the team need no lock
	if kind >= powerupKinds {
		return View{}, ErrUnknownPowerup
	}
	if kind == PowerupInstantCast {
		return View{}, errNeedsPattern
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
	cur := s.catchUp(e, t.ID)
	now := s.now()

	// 2. validate (state)
	if !s.isLeader(t.ID, userID) {
		return View{}, ErrNotLeader
	}
	if cur.Boss.KnockedOut() {
		return View{}, ErrKnockedOut
	}
	if !cur.Powerups.Held.Has(kind) {
		return View{}, ErrPowerupNotHeld
	}
	switch {
	case kind == PowerupDamageMultiplier && cur.Powerups.MultiplierArmed,
		kind == PowerupShortQRCooldown && now.Before(cur.Powerups.ShortQRUntil):
		return View{}, ErrPowerupActive
	case kind == PowerupHeart && cur.Boss.Health >= s.rules.Health:
		return View{}, ErrFullHealth
	}

	// 3. build
	next := *cur
	next.Powerups.Held = cur.Powerups.Held.Without(kind)
	var activatedCasts *int
	switch kind {
	case PowerupDamageMultiplier:
		next.Powerups.MultiplierArmed = true
		n := len(cur.Casts) // armed while the log stays this long
		activatedCasts = &n
	case PowerupHeart:
		// More phase health, from the same phase start: the attacks so far
		// still count, so health rises by heal and the clock doesn't move.
		// Safe for the knockout cap: the team isn't knocked out, so fewer
		// attacks than the cap have happened, and a higher PhaseHealth only
		// raises it.
		heal := min(s.rules.PowerupHeart, s.rules.Health-cur.Boss.Health)
		next.Boss.PhaseHealth += heal
		next.Boss.Health += heal
	case PowerupShortQRCooldown:
		next.Powerups.ShortQRUntil = now.Add(s.rules.ShortQRWindow)
	}
	next.Version = cur.Version.Next(s.rules.Epoch)

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		if kind == PowerupHeart {
			return s.db.useHeart(ctx, t.ID, now, next.Boss)
		}
		return s.db.updatePowerupActivated(ctx, t.ID, kind.ID(), now, activatedCasts)
	}); err != nil {
		return View{}, s.reconcile(e, t.ID, err)
	}

	// 5. apply
	e.cur.Store(&next)
	// 6. publish, and re-arm the timer: a short_qr_cooldown's window ends
	// at its own time
	s.publish(e, t.ID, &next, PowerupActivated)
	s.schedule(e, t.ID, &next)
	return s.view(t.ID, next), nil
}

// InstantCast uses userID's team's held instant_cast powerup: the pattern
// made of waypointIDs is cast at once, with no scans, whatever the class and
// QR cooldowns; then it's a cast like any other (applyCast: damage, × an
// armed multiplier, last_cast, cooldowns, a boss defeat or the team
// finishing; the selection and stack are purged). Only the leader casts.
// Refused, in order: ErrUnknownWaypoint, ErrUnknownPattern, ErrNotInTeam,
// ErrNotInGame, ErrNotLeader, ErrKnockedOut, ErrPowerupNotHeld,
// ErrPatternLocked (bigger than the team has unlocked).
func (s *Store) InstantCast(ctx context.Context, userID uuid.UUID, waypointIDs []string) (View, error) {
	// 2. validate, before 1. lock: content and the team need no lock
	set, err := s.atlas.SetOf(waypointIDs)
	if err != nil {
		return View{}, ErrUnknownWaypoint
	}
	p, ok := s.atlas.PatternFor(set)
	if !ok {
		return View{}, ErrUnknownPattern
	}
	t, err := s.inGameTeam(userID)
	if err != nil {
		return View{}, err
	}
	// A cast that defeats the last boss finishes the team, once the lock
	// below is released (Rules.OnFinished: no lock held while teams works).
	var finished bool
	defer func() {
		if finished && s.rules.OnFinished != nil {
			s.rules.OnFinished(t.ID)
		}
	}()

	// 1. lock, then 2. validate membership again under it: an admin may
	// have removed the user since the lookup.
	e := s.lockTeam(t.ID)
	defer e.mu.Unlock()
	if !s.isMember(t.ID, userID) {
		return View{}, ErrNotInTeam
	}
	cur := s.catchUp(e, t.ID)
	now := s.now()

	// 2. validate (state): deliberately no class or QR cooldown check
	if !s.isLeader(t.ID, userID) {
		return View{}, ErrNotLeader
	}
	if cur.Boss.KnockedOut() {
		return View{}, ErrKnockedOut
	}
	if !cur.Powerups.Held.Has(PowerupInstantCast) {
		return View{}, ErrPowerupNotHeld
	}
	if s.atlas.Pattern(p).Waypoints.Len() > s.unlockedPoints(*cur) {
		return View{}, ErrPatternLocked
	}

	// 3. build
	next := *cur
	next.Powerups.Held = cur.Powerups.Held.Without(PowerupInstantCast)
	cast, typ, last := s.applyCast(cur, &next, p, userID, now, true)
	next.Version = cur.Version.Next(s.rules.Epoch)

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		return s.db.insertInstantCast(ctx, t.ID, s.atlas.Pattern(p).ID, cast, next.Boss)
	}); err != nil {
		return View{}, s.reconcile(e, t.ID, err)
	}

	// 5. apply
	e.cur.Store(&next)
	finished = last
	// 6. publish, and re-arm the timer: a defeat starts the next boss's
	// clock, and the cast's QR cooldowns end at their own time
	s.publish(e, t.ID, &next, typ)
	s.schedule(e, t.ID, &next)
	return s.view(t.ID, next), nil
}

// AnswerQTE records userID's answer for their team to quick-time event
// qteID, choosing option: any member may answer, one answer per team. It's
// right or wrong at once, and a right one gives the team the QTE's powerup
// in the same transaction (wasted if it already holds that kind). Refused,
// in order: ErrUnknownQTE (no such QTE, or a draft), ErrInvalidOption,
// ErrNotInTeam, ErrNotInGame, ErrQTEAlreadyAnswered (checked before the
// close, so a resend after it still lands here), ErrQTEClosed (now at or
// past its ends_at), ErrKnockedOut.
func (s *Store) AnswerQTE(ctx context.Context, userID uuid.UUID, qteID int64, option int) (View, error) {
	// 2. validate, before 1. lock: the QTE's content never changes, and the
	// team needs no lock
	q, ok := s.qtes.ByID(qteID)
	if !ok || q.StartedAt == nil || q.Quiz == nil {
		return View{}, ErrUnknownQTE
	}
	if option < 0 || option >= len(q.Quiz.Options) {
		return View{}, ErrInvalidOption
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
	cur := s.catchUp(e, t.ID)
	now := s.now()

	// 2. validate (state)
	if slices.ContainsFunc(cur.QTEAnswers, func(a QTEAnswer) bool { return a.QTE == qteID }) {
		return View{}, ErrQTEAlreadyAnswered
	}
	q, _ = s.qtes.ByID(qteID) // again: an admin may have ended it since
	if !now.Before(*q.EndsAt) {
		return View{}, ErrQTEClosed
	}
	if cur.Boss.KnockedOut() {
		return View{}, ErrKnockedOut
	}

	// 3. build
	kind, known := PowerupByID(q.Powerup) // qte checks it against PowerupIDs
	a := QTEAnswer{QTE: qteID, Option: option, Correct: option == q.Quiz.CorrectOption, By: userID, At: now}
	a.Granted = a.Correct && known && !cur.Powerups.Held.Has(kind)
	next := *cur
	next.QTEAnswers = append(slices.Clip(cur.QTEAnswers), a) // copy-on-write
	grant := ""
	if a.Granted {
		next.Powerups.Held = cur.Powerups.Held.With(kind)
		grant = kind.ID()
	}
	next.Version = cur.Version.Next(s.rules.Epoch)

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		return s.db.insertQTEAnswer(ctx, t.ID, a, grant)
	}); err != nil {
		return View{}, s.reconcile(e, t.ID, err)
	}

	// 5. apply
	e.cur.Store(&next)
	// 6. publish
	s.publish(e, t.ID, &next, QTEAnswered)
	return s.view(t.ID, next), nil
}
