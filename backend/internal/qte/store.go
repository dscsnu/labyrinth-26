package qte

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres"
)

// Store layout:
//
//	Store
//	  rules, db, hub, log, clock       set once by Load
//	  mu (Mutex)                       writers and the close timer queue here;
//	                                   broadcasts go out under it, in order
//	  all  atomic.Pointer[[]QTE]       every QTE, newest first; never modified:
//	                                   a write stores a new slice. Read lock-free
//	  stopTimer func() bool            cancels the pending close timer (mu)
//	  armedFor  int64                  the QTE the timer will close, 0 if none (mu)
//	  closed    bool                   set by Close: no timer arms or acts (mu)
//
// There is one entry, not one per key: QTEs run one at a time, and there are
// tens of them, not thousands.
type Store struct {
	rules Rules
	db    db
	hub   *live.Hub
	log   *slog.Logger
	clock Clock

	mu        sync.Mutex
	all       atomic.Pointer[[]QTE]
	stopTimer func() bool
	armedFor  int64
	closed    bool
}

// Clock is where the store gets time: Now for stamping and deciding what's
// open, AfterFunc for the close timer. Tests pass a fake whose timers fire
// only when the test moves its time.
type Clock interface {
	// Now returns the current time, UTC, truncated to microseconds
	// (Postgres's precision), so a restart reproduces times exactly.
	Now() time.Time
	// AfterFunc calls f in its own goroutine after d; stop cancels it.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

// systemClock is the real Clock.
type systemClock struct{}

// Now returns time.Now in UTC, truncated to microseconds.
func (systemClock) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// AfterFunc is time.AfterFunc.
func (systemClock) AfterFunc(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }

// Rules are the settings from config.toml, and what server.Build passes in.
type Rules struct {
	// AnswerWindow is how long a QTE stays open once started, unless it has
	// its own window (qte.answer_window).
	AnswerWindow time.Duration
	// MaxAnswerWindow is the longest window an admin may give one QTE
	// (qte.max_answer_window).
	MaxAnswerWindow time.Duration
	// Powerups are the powerup kinds a QTE may earn: teamstate's
	// (teamstate.PowerupIDs), passed in so qte needn't import teamstate.
	Powerups []string
	// WriteTimeout bounds each write-through (storage.write_timeout).
	WriteTimeout time.Duration
	// Clock is the store's time source; nil means the real one
	// (systemClock). Tests pass a fake. Not in config.toml.
	Clock Clock
}

// The event types of the game-wide "qte" event (besides live.Snapshot and
// live.Resync).
const (
	// Started: an admin started a QTE; the message is it, open, without its
	// answer.
	Started live.EventType = "started"
	// Closed: the open QTE closed, its window over or ended by an admin; the
	// message is it, closed, with its answer revealed. After this the
	// resource is null until the next QTE starts.
	Closed live.EventType = "closed"
)

// eventName is the game-wide event's name, the resource it carries.
const eventName = "qte"

// Load reads every QTE from Postgres, checking each against rules (a kind or
// powerup it doesn't know refuses the start), and arms the close timer if
// one is open. Call once at startup; Close stops the timer.
func Load(ctx context.Context, pg *postgres.DB, hub *live.Hub, log *slog.Logger, rules Rules) (*Store, error) {
	s := &Store{rules: rules, db: db{pool: pg.Pool()}, hub: hub, log: log, clock: rules.Clock}
	if s.clock == nil {
		s.clock = systemClock{}
	}
	all, err := s.db.selectQTEs(ctx)
	if err != nil {
		return nil, fmt.Errorf("load qte: %w", err)
	}
	for _, q := range all {
		if err := s.check(q); err != nil {
			return nil, fmt.Errorf("qte %d: %w", q.ID, err)
		}
	}
	s.all.Store(&all)
	s.mu.Lock()
	s.schedule()
	s.mu.Unlock()
	log.Info("loaded qtes", "qtes", len(all))
	return s, nil
}

// check refuses a stored QTE the running code can't serve: an unknown kind
// or powerup (the database CHECKs list the same, so only a content change
// between restarts can trip it).
func (s *Store) check(q QTE) error {
	if q.Kind != KindQuiz || q.Quiz == nil {
		return fmt.Errorf("unknown kind %q", q.Kind)
	}
	if !slices.Contains(s.rules.Powerups, q.Powerup) {
		return fmt.Errorf("unknown powerup %q", q.Powerup)
	}
	return nil
}

// Close stops the close timer; none arms or acts afterwards. Call on
// shutdown, and before a test loads another store on the same database.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopTimer != nil {
		s.stopTimer()
		s.stopTimer = nil
	}
	s.armedFor, s.closed = 0, true
}

// now is the clock's time.
func (s *Store) now() time.Time { return s.clock.Now() }

// persist runs a db.go write detached from the request (a client leaving
// mustn't leave a commit's outcome unknown) and bounded by
// Rules.WriteTimeout.
func (s *Store) persist(ctx context.Context, write func(context.Context) error) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.rules.WriteTimeout)
	defer cancel()
	return write(wctx)
}

// reconcile reloads every QTE after a failed write (Postgres wins),
// broadcasts the open one as resync, re-arms the timer, and returns
// writeErr. If the reload fails too, memory stays as it was. The caller holds
// mu.
func (s *Store) reconcile(writeErr error) error {
	s.log.Error("qte write failed; reloading", "error", writeErr)
	ctx, cancel := context.WithTimeout(context.Background(), s.rules.WriteTimeout)
	defer cancel()
	all, err := s.db.selectQTEs(ctx)
	if err != nil {
		s.log.Error("qte reload failed", "error", err)
		return writeErr
	}
	s.all.Store(&all)
	s.broadcast(live.Resync, s.public(s.now()))
	s.schedule()
	return writeErr
}

// schedule arms the close timer for the open QTE's ends_at, replacing any
// pending one; nothing if none is open or the store is closed. The caller
// holds mu.
func (s *Store) schedule() {
	if s.stopTimer != nil {
		s.stopTimer()
		s.stopTimer = nil
	}
	s.armedFor = 0
	if s.closed {
		return
	}
	now := s.now()
	q, ok := s.open(now)
	if !ok {
		return
	}
	id := q.ID
	s.armedFor = id
	s.stopTimer = s.clock.AfterFunc(q.EndsAt.Sub(now), func() { s.due(id) })
}

// due is the close timer's callback for QTE id: once its ends_at has passed
// it broadcasts it closed, with its answer. It writes nothing: a QTE is
// closed because the clock passed its stored ends_at, which every read
// already sees, so Load reproduces it. A timer that fires early re-arms; one
// for a QTE that was ended (or replaced) meanwhile does nothing.
func (s *Store) due(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.armedFor != id {
		return
	}
	q, _ := s.ByID(id)
	if now := s.now(); now.Before(*q.EndsAt) {
		s.schedule()
		return
	}
	s.armedFor, s.stopTimer = 0, nil
	s.broadcast(Closed, publicView(q, true))
}

// broadcast sends the "qte" event of type typ with message v (nil: no QTE
// open) to every stream. The caller holds mu, so broadcasts go out in the
// order the changes were applied.
func (s *Store) broadcast(typ live.EventType, v *View) {
	ev, err := live.NewEvent(eventName, typ, v)
	if err != nil {
		s.log.Error("encode qte event", "error", err)
		return
	}
	s.hub.Broadcast(ev)
}
