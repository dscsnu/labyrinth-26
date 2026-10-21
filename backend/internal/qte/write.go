package qte

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Every write runs the write-through steps of AGENTS.md §6, numbered in the
// code: 1. lock (mu), 2. validate, 3. build, 4. persist, 5. apply,
// 6. publish (a broadcast, under mu). Create persists before it builds
// (variation b): Postgres sets id and created_at. Draft validation needs no
// lock and runs first (variation a).

// MinOptions is the fewest options a quiz may have: with one there's
// nothing to choose. A correctness bound, not a tunable, so not in
// config.toml (AGENTS.md §10).
const MinOptions = 2

var (
	// ErrUnknownQTE means no QTE has that ID.
	ErrUnknownQTE = errors.New("unknown qte")
	// ErrQTEOpen means another QTE is open: one runs at a time.
	ErrQTEOpen = errors.New("another qte is open")
	// ErrQTEAlreadyRun means the QTE has been started before: each runs once
	// (create a copy to ask again).
	ErrQTEAlreadyRun = errors.New("this qte has already run")
	// ErrQTENotStarted means the QTE is a draft: there's nothing to end.
	ErrQTENotStarted = errors.New("this qte hasn't started")
	// ErrQTEClosed means the QTE's window is over.
	ErrQTEClosed = errors.New("this qte is closed")
	// ErrUnknownKind means a kind other than "quiz".
	ErrUnknownKind = errors.New("unknown qte kind")
	// ErrUnknownPowerup means a powerup kind teamstate doesn't have.
	ErrUnknownPowerup = errors.New("unknown powerup")
	// ErrInvalidQuestion means a quiz with no question (or only spaces).
	ErrInvalidQuestion = errors.New("a quiz needs a question")
	// ErrInvalidOptions means fewer than MinOptions options, or an empty or
	// repeated one.
	ErrInvalidOptions = errors.New("a quiz needs at least 2 different, non-empty options")
	// ErrInvalidCorrectOption means the right option's index isn't one of
	// the options.
	ErrInvalidCorrectOption = errors.New("the correct option must be one of the options")
	// ErrInvalidWindow means a window under 1 s or over qte.max_answer_window.
	ErrInvalidWindow = errors.New("the window must be from 1 s to qte.max_answer_window")
)

// Draft is a QTE as an admin writes it.
type Draft struct {
	// Kind is what it is: "quiz".
	Kind string `json:"kind"`
	// Powerup is the powerup kind a correct answer earns.
	Powerup string `json:"powerup"`
	// Quiz is the quiz, for a quiz.
	Quiz *Quiz `json:"quiz"`
	// WindowS is its own window in seconds; nil for qte.answer_window.
	WindowS *int `json:"window_s"`
}

// validate checks d against the rules; the errors are in that order.
func (s *Store) validate(d Draft) error {
	if d.Kind != KindQuiz || d.Quiz == nil {
		return ErrUnknownKind
	}
	if !slices.Contains(s.rules.Powerups, d.Powerup) {
		return ErrUnknownPowerup
	}
	q := d.Quiz
	if strings.TrimSpace(q.Question) == "" {
		return ErrInvalidQuestion
	}
	if len(q.Options) < MinOptions {
		return ErrInvalidOptions
	}
	for i, o := range q.Options {
		if strings.TrimSpace(o) == "" || slices.Contains(q.Options[:i], o) {
			return ErrInvalidOptions
		}
	}
	if q.CorrectOption < 0 || q.CorrectOption >= len(q.Options) {
		return ErrInvalidCorrectOption
	}
	// In seconds: a huge window_s would overflow a time.Duration and wrap
	// past the check.
	if w := d.WindowS; w != nil && (*w < 1 || *w > int(s.rules.MaxAnswerWindow/time.Second)) {
		return ErrInvalidWindow
	}
	return nil
}

// window returns how long q stays open once started: its own window, else
// Rules.AnswerWindow.
func (s *Store) window(q QTE) time.Duration {
	if q.WindowS != nil {
		return time.Duration(*q.WindowS) * time.Second
	}
	return s.rules.AnswerWindow
}

// Create writes a new QTE by adminID from d: a draft to start later, or,
// with start, started at once (open for its window). Refused, in order: the
// draft's errors (validate), then ErrQTEOpen when starting while another is
// open.
func (s *Store) Create(ctx context.Context, adminID uuid.UUID, d Draft, start bool) (QTE, error) {
	// 2. validate, before 1. lock: the draft alone
	if err := s.validate(d); err != nil {
		return QTE{}, err
	}
	quiz := *d.Quiz
	quiz.Options = slices.Clone(quiz.Options)
	q := QTE{Kind: d.Kind, Powerup: d.Powerup, Quiz: &quiz, WindowS: d.WindowS, CreatedBy: &adminID}

	// 1. lock
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	// 2. validate
	if start {
		if _, open := s.open(now); open {
			return QTE{}, ErrQTEOpen
		}
		endsAt := now.Add(s.window(q))
		q.StartedAt, q.EndsAt = &now, &endsAt
	}

	// 4. persist, before 3. build: Postgres sets id and created_at
	var err error
	if err = s.persist(ctx, func(ctx context.Context) error {
		q.ID, q.CreatedAt, err = s.db.insertQTE(ctx, q, adminID)
		return err
	}); err != nil {
		if errors.Is(err, errOverlap) {
			return QTE{}, errors.Join(ErrQTEOpen, s.reconcile(err))
		}
		return QTE{}, s.reconcile(err)
	}

	// 3. build, 5. apply: the new QTE first (newest first)
	s.all.Store(new(append([]QTE{q}, *s.all.Load()...)))
	// 6. publish
	if start {
		s.broadcast(Started, publicView(q, false))
		s.schedule()
	}
	return q, nil
}

// Start opens draft QTE id now, for its window. Refused, in order:
// ErrUnknownQTE, ErrQTEAlreadyRun (started before), ErrQTEOpen (another is
// open).
func (s *Store) Start(ctx context.Context, id int64) (QTE, error) {
	// 1. lock
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	// 2. validate
	cur, ok := s.ByID(id)
	if !ok {
		return QTE{}, ErrUnknownQTE
	}
	if cur.StartedAt != nil {
		return QTE{}, ErrQTEAlreadyRun
	}
	if _, open := s.open(now); open {
		return QTE{}, ErrQTEOpen
	}

	// 3. build
	q := cur
	endsAt := now.Add(s.window(q))
	q.StartedAt, q.EndsAt = &now, &endsAt

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		return s.db.updateQTEStarted(ctx, id, now, endsAt)
	}); err != nil {
		if errors.Is(err, errOverlap) {
			return QTE{}, errors.Join(ErrQTEOpen, s.reconcile(err))
		}
		return QTE{}, s.reconcile(err)
	}

	// 5. apply
	s.replace(q)
	// 6. publish, and arm the close timer
	s.broadcast(Started, publicView(q, false))
	s.schedule()
	return q, nil
}

// End closes open QTE id now, before its window is over, and broadcasts it
// closed with its answer. Refused, in order: ErrUnknownQTE,
// ErrQTENotStarted (a draft), ErrQTEClosed (already over).
func (s *Store) End(ctx context.Context, id int64) (QTE, error) {
	// 1. lock
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	// 2. validate
	cur, ok := s.ByID(id)
	if !ok {
		return QTE{}, ErrUnknownQTE
	}
	switch cur.Status(now) {
	case StatusDraft:
		return QTE{}, ErrQTENotStarted
	case StatusClosed:
		return QTE{}, ErrQTEClosed
	}

	// 3. build
	q := cur
	q.EndsAt = &now

	// 4. persist
	if err := s.persist(ctx, func(ctx context.Context) error {
		return s.db.updateQTEEnded(ctx, id, now)
	}); err != nil {
		return QTE{}, s.reconcile(err)
	}

	// 5. apply
	s.replace(q)
	// 6. publish the close (the answer revealed), and drop the timer
	s.broadcast(Closed, publicView(q, true))
	s.schedule()
	return q, nil
}

// replace stores a new list with q in place of the QTE with its ID (a new
// slice: the old one may be read concurrently). The caller holds mu.
func (s *Store) replace(q QTE) {
	all := slices.Clone(*s.all.Load())
	for i := range all {
		if all[i].ID == q.ID {
			all[i] = q
		}
	}
	s.all.Store(&all)
}
