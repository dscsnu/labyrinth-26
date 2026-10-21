package qte

import (
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/platform/live"
)

// KindQuiz is the quiz kind: a question with options, one of them right.
// The kinds are fixed in code (each is a rule of its own) and listed in the
// qte table's CHECK.
const KindQuiz = "quiz"

// The statuses of a QTE, derived from the clock: never stored.
const (
	// StatusDraft: never started.
	StatusDraft = "draft"
	// StatusOpen: started, and now is before its ends_at.
	StatusOpen = "open"
	// StatusClosed: its ends_at has passed.
	StatusClosed = "closed"
)

// QTE is a quick-time event, in full: its answer included. Only admins see
// it whole; players get View.
type QTE struct {
	// ID is qte.id.
	ID int64 `json:"id"`
	// Kind is what it is: KindQuiz.
	Kind string `json:"kind"`
	// Powerup is the powerup kind a correct answer earns.
	Powerup string `json:"powerup"`
	// Quiz is the quiz, for KindQuiz.
	Quiz *Quiz `json:"quiz"`
	// WindowS is its own window in seconds, or nil for qte.answer_window.
	WindowS *int `json:"window_s"`
	// CreatedBy is the admin who wrote it, or nil for a seeded one.
	CreatedBy *uuid.UUID `json:"created_by"`
	// CreatedAt is when it was written.
	CreatedAt time.Time `json:"created_at"`
	// StartedAt is when it was started, or nil while a draft.
	StartedAt *time.Time `json:"started_at"`
	// EndsAt is when answers stop, or nil while a draft.
	EndsAt *time.Time `json:"ends_at"`
}

// Quiz is a quiz QTE's content.
type Quiz struct {
	// Question is what's asked.
	Question string `json:"question"`
	// Options are the choices, in order; an answer names one by index.
	Options []string `json:"options"`
	// CorrectOption is the index of the right choice.
	CorrectOption int `json:"correct_option"`
}

// Status returns q's status at now: StatusDraft, StatusOpen or
// StatusClosed.
func (q QTE) Status(now time.Time) string {
	switch {
	case q.StartedAt == nil:
		return StatusDraft
	case now.Before(*q.EndsAt):
		return StatusOpen
	default:
		return StatusClosed
	}
}

// ByID returns QTE id; ok is false if there's none. Lock-free: what
// teamstate calls to check an answer.
func (s *Store) ByID(id int64) (QTE, bool) {
	for _, q := range *s.all.Load() {
		if q.ID == id {
			return q, true
		}
	}
	return QTE{}, false
}

// List returns every QTE, newest first. Lock-free.
func (s *Store) List() []QTE { return *s.all.Load() }

// open returns the QTE open at now; ok is false if none is.
func (s *Store) open(now time.Time) (QTE, bool) {
	for _, q := range *s.all.Load() {
		if q.Status(now) == StatusOpen {
			return q, true
		}
	}
	return QTE{}, false
}

// View is a QTE as players see it: the message of the "qte" event and of GET
// /api/qte. Kind-specific content sits under its kind's key.
type View struct {
	// ID is the QTE's ID: what an answer names.
	ID int64 `json:"id"`
	// Kind is what it is: "quiz".
	Kind string `json:"kind"`
	// Status is "open", or "closed" in the closing event.
	Status string `json:"status"`
	// Powerup is the powerup kind a correct answer earns.
	Powerup string `json:"powerup"`
	// StartedAt is when it opened.
	StartedAt time.Time `json:"started_at"`
	// EndsAt is when answers stop. Clients count down to it on the server's
	// clock (hello).
	EndsAt time.Time `json:"ends_at"`
	// Quiz is the quiz, for a quiz.
	Quiz *QuizView `json:"quiz,omitempty"`
}

// QuizView is a quiz as players see it.
type QuizView struct {
	// Question is what's asked.
	Question string `json:"question"`
	// Options are the choices, in order: answer with an index.
	Options []string `json:"options"`
	// CorrectOption is the right choice's index once closed; null while
	// open, so the answer never travels before the end.
	CorrectOption *int `json:"correct_option"`
}

// publicView is q as players see it; closed adds the answer. The one place
// a player-facing QTE is built, so the answer can't leak through another.
func publicView(q QTE, closed bool) *View {
	v := &View{ID: q.ID, Kind: q.Kind, Status: StatusOpen, Powerup: q.Powerup,
		StartedAt: q.StartedAt.UTC(), EndsAt: q.EndsAt.UTC()}
	if closed {
		v.Status = StatusClosed
	}
	if q.Quiz != nil {
		v.Quiz = &QuizView{Question: q.Quiz.Question, Options: q.Quiz.Options}
		if closed {
			correct := q.Quiz.CorrectOption
			v.Quiz.CorrectOption = &correct
		}
	}
	return v
}

// public returns the QTE open at now as players see it, or nil if none is.
func (s *Store) public(now time.Time) *View {
	if q, ok := s.open(now); ok {
		return publicView(q, false)
	}
	return nil
}

// Public returns the open QTE as players see it (no answer), or nil if none
// is open: GET /api/qte. Lock-free.
func (s *Store) Public() *View { return s.public(s.now()) }

// Snapshot returns the "qte" event for a new stream: the open QTE, or null.
func (s *Store) Snapshot() (live.Event, error) {
	return live.NewEvent(eventName, live.Snapshot, s.Public())
}
