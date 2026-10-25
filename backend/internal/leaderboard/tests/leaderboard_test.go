package tests

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/leaderboard"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
)

// fakeStandings is a leaderboard.Standings with a settable list.
type fakeStandings struct{ list []teamstate.Standing }

// Standings returns the list.
func (f *fakeStandings) Standings() []teamstate.Standing { return f.list }

// fakeTeams is a leaderboard.Teams: the teams, and which user is in which.
type fakeTeams struct {
	list    []teams.Team
	members map[uuid.UUID]string
}

// All returns the teams.
func (f *fakeTeams) All() []teams.Team { return f.list }

// ForUser returns userID's team, or ErrNotInTeam.
func (f *fakeTeams) ForUser(userID uuid.UUID) (teams.Team, error) {
	for _, t := range f.list {
		if t.ID == f.members[userID] {
			return t, nil
		}
	}
	return teams.Team{}, teams.ErrNotInTeam
}

// base is a fixed time the standings are measured from.
var base = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// league returns standings and teams that exercise every key: more bosses
// first; on equal bosses, the earlier last defeat; a team with no defeat
// after any with one; then more damage; then team ID. A lobby team isn't
// ranked; a finished one is marked.
func league() (*fakeStandings, *fakeTeams) {
	fs := &fakeStandings{list: []teamstate.Standing{
		{TeamID: "100001", BossesDefeated: 2, LastDefeatAt: base.Add(10 * time.Minute), Damage: 5},
		{TeamID: "100002", BossesDefeated: 2, LastDefeatAt: base.Add(5 * time.Minute), Damage: 0}, // earlier: first
		{TeamID: "100003", BossesDefeated: 6, LastDefeatAt: base.Add(time.Hour)},                  // finished
		{TeamID: "100004", BossesDefeated: 0, Damage: 40},
		{TeamID: "100005", BossesDefeated: 0, Damage: 40}, // a tie with 100004: team ID decides
		{TeamID: "100006", BossesDefeated: 0, Damage: 90},
		// 100007 has no standing (did nothing): zero
	}}
	ft := &fakeTeams{list: []teams.Team{
		{ID: "100001", Name: "A", Status: teams.InGame}, {ID: "100002", Name: "B", Status: teams.InGame},
		{ID: "100003", Name: "C", Status: teams.Finished}, {ID: "100004", Name: "D", Status: teams.InGame},
		{ID: "100005", Name: "E", Status: teams.InGame}, {ID: "100006", Name: "F", Status: teams.InGame},
		{ID: "100007", Name: "G", Status: teams.InGame}, {ID: "100008", Name: "Lobby", Status: teams.Lobby},
	}, members: map[uuid.UUID]string{}}
	return fs, ft
}

// newBoard returns a leaderboard over fs and ft with size, and its hub.
func newBoard(fs *fakeStandings, ft *fakeTeams, size int) (*leaderboard.Leaderboard, *live.Hub) {
	hub := live.NewHub(1, 16)
	return leaderboard.New(fs, ft, hub, slog.New(slog.NewTextHandler(io.Discard, nil)), leaderboard.Rules{Size: size, PublishEvery: time.Second}), hub
}

// TestRanking checks the order and every tie-break, ranks from 1, the
// finished flag, the lobby team left out, the top cut at size, and the
// caller's own entry (even below the top; null with no started team).
func TestRanking(t *testing.T) {
	fs, ft := league()
	me := uuid.New()
	ft.members[me] = "100005"
	b, _ := newBoard(fs, ft, 3)
	got := b.ForUser(me)
	if got.Teams != 7 || len(got.Top) != 3 {
		t.Fatalf("teams %d, top %d", got.Teams, len(got.Top))
	}
	want := []string{"100003", "100002", "100001"}
	for i, e := range got.Top {
		if e.TeamID != want[i] || e.Rank != i+1 {
			t.Fatalf("top[%d] = %s rank %d, want %s rank %d", i, e.TeamID, e.Rank, want[i], i+1)
		}
	}
	if !got.Top[0].Finished || got.Top[1].Finished || got.Top[0].LastDefeatAt == nil {
		t.Fatalf("finished / last defeat: %+v", got.Top[:2])
	}
	// The rest: 100006 (90 damage), 100004 and 100005 (40, by ID), 100007.
	if got.Me == nil || got.Me.TeamID != "100005" || got.Me.Rank != 6 || got.Me.LastDefeatAt != nil {
		t.Fatalf("me: %+v, want 100005 at rank 6", got.Me)
	}
	if none := b.ForUser(uuid.New()); none.Me != nil {
		t.Fatalf("a user with no team: %+v", none.Me)
	}
}

// TestRefreshBroadcastsOnChange checks Refresh: the first call broadcasts
// the top (type updated), an unchanged top doesn't broadcast again, a change
// in it does, and a change below the top doesn't. Snapshot carries the top.
func TestRefreshBroadcastsOnChange(t *testing.T) {
	fs, ft := league()
	b, hub := newBoard(fs, ft, 2)
	sub := hub.Subscribe(uuid.New())
	defer sub.Close()
	if !b.Refresh() {
		t.Fatal("the first refresh should broadcast")
	}
	ev := <-sub.C
	var env struct {
		Type    string            `json:"type"`
		Message leaderboard.Board `json:"message"`
	}
	if err := json.Unmarshal(ev.Data(), &env); err != nil || ev.Name() != "leaderboard" || env.Type != "updated" ||
		len(env.Message.Top) != 2 || env.Message.Top[0].TeamID != "100003" {
		t.Fatalf("event %s: %s (%v)", ev.Name(), ev.Data(), err)
	}
	if b.Refresh() {
		t.Fatal("an unchanged top broadcast again")
	}
	fs.list[3].Damage = 41 // 100004, below the top two
	if b.Refresh() {
		t.Fatal("a change below the top broadcast")
	}
	fs.list[0].BossesDefeated = 7 // 100001 takes the lead
	if !b.Refresh() {
		t.Fatal("a change in the top didn't broadcast")
	}
	snap, err := b.Snapshot()
	if err != nil || snap.Type() != live.Snapshot {
		t.Fatalf("snapshot: %v %s", err, snap.Type())
	}
}

// TestFullRankingForAdmins checks Rules.SendAdmins: Refresh sends every
// started team (past leaderboard.size) as "admin_leaderboard" of type
// updated, not again while unchanged, and again on a change below the top
// that the players' board ignores; AdminSnapshot carries the full ranking.
func TestFullRankingForAdmins(t *testing.T) {
	fs, ft := league()
	var sent []live.Event
	b := leaderboard.New(fs, ft, live.NewHub(1, 16), slog.New(slog.NewTextHandler(io.Discard, nil)), leaderboard.Rules{
		Size: 2, PublishEvery: time.Second, SendAdmins: func(ev live.Event) { sent = append(sent, ev) },
	})
	full := func(ev live.Event) leaderboard.Full {
		t.Helper()
		var env struct {
			Type    string           `json:"type"`
			Message leaderboard.Full `json:"message"`
		}
		if err := json.Unmarshal(ev.Data(), &env); err != nil || ev.Name() != "admin_leaderboard" {
			t.Fatalf("event %s: %s (%v)", ev.Name(), ev.Data(), err)
		}
		return env.Message
	}
	b.Refresh()
	if len(sent) != 1 || sent[0].Type() != leaderboard.Updated || len(full(sent[0]).Teams) != 7 || full(sent[0]).Teams[6].TeamID != "100007" {
		t.Fatalf("first refresh sent %d", len(sent))
	}
	b.Refresh()
	if len(sent) != 1 {
		t.Fatal("an unchanged ranking was sent again")
	}
	fs.list[3].Damage = 41 // 100004, below the players' top two
	if b.Refresh() || len(sent) != 2 {
		t.Fatalf("a change below the top: players' broadcast or admins' %d", len(sent))
	}
	snap, err := b.AdminSnapshot()
	if err != nil || snap.Type() != live.Snapshot || len(full(snap).Teams) != 7 {
		t.Fatalf("admin snapshot: %v %s", err, snap.Type())
	}
}
