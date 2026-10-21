package tests

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/qte"
	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
)

// TestResetCooldowns casts the quadrilateral (class_4, its four waypoints on QR
// cooldown), then resets the team's cooldowns: cooldown_class goes null and
// qr_cooldowns empty (published as cooldowns_reset), the quadrilateral can be
// selected again and its waypoints scanned; a restart keeps the reset
// (stored); and the next cast starts cooldowns as usual.
func TestResetCooldowns(t *testing.T) {
	f := started(t, fixtureOptions{qrCooldown: true})
	if f.rules.QRCooldown <= 0 {
		t.Skip("needs game.qr_cooldown > 0")
	}
	sub := f.hub.Subscribe(f.users[0])
	defer sub.Close()
	f.castQuad(t, 0)
	if v := f.view(t); v.CooldownClass == nil || len(v.QRCooldowns) != len(quad) {
		t.Fatalf("after the quadrilateral: cooldown %v, qr %v", v.CooldownClass, v.QRCooldowns)
	}
	if _, err := f.state.Select(context.Background(), f.users[0], "quadrilateral"); !errors.Is(err, teamstate.ErrShapeOnCooldown) {
		t.Fatalf("the quadrilateral before the reset: %v", err)
	}
	for len(sub.C) > 0 {
		<-sub.C
	}

	v := pgtest.Must(f.state.ResetCooldowns(context.Background(), f.teamID))
	if v.CooldownClass != nil || len(v.QRCooldowns) != 0 || len(v.CooldownShapes) != 0 {
		t.Fatalf("after the reset: cooldown %v, qr %v, shapes %v", v.CooldownClass, v.QRCooldowns, v.CooldownShapes)
	}
	expectType(t, sub, "reset", teamstate.CooldownsReset)

	f.restart(t)
	if v := f.view(t); v.CooldownClass != nil || len(v.QRCooldowns) != 0 {
		t.Fatalf("the reset across a restart: cooldown %v, qr %v", v.CooldownClass, v.QRCooldowns)
	}
	res := f.castQuad(t, 1) // allowed again: neither its class nor its waypoints cool down
	if v := res.State; v.CooldownClass == nil || len(v.QRCooldowns) != len(quad) {
		t.Fatalf("a cast after the reset starts new cooldowns: cooldown %v, qr %v", v.CooldownClass, v.QRCooldowns)
	}
}

// TestResetCooldownsErrors checks ResetCooldowns' refusals: an unknown team,
// and a team that hasn't started.
func TestResetCooldownsErrors(t *testing.T) {
	f := started(t, fixtureOptions{lobby: true})
	if _, err := f.state.ResetCooldowns(context.Background(), "000000"); !errors.Is(err, teamstate.ErrTeamNotFound) {
		t.Fatalf("unknown team: %v", err)
	}
	if _, err := f.state.ResetCooldowns(context.Background(), f.teamID); !errors.Is(err, teamstate.ErrNotInGame) {
		t.Fatalf("lobby team: %v", err)
	}
}

// TestFinishedHook gives every boss 1 HP and casts until the last falls:
// Rules.OnFinished is called exactly once, with the team, and only by the
// cast that defeated the last boss. It's called after the team's lock is
// released: the hook itself takes that lock (Revive), which would deadlock
// otherwise. A restart that finds the team still IN_GAME with every boss
// defeated calls it again (the mark was never made).
func TestFinishedHook(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var f *fixtureEnv
	hook := func(teamID string) {
		pgtest.Must(f.state.Revive(context.Background(), teamID)) // takes the team's lock
		mu.Lock()
		calls = append(calls, teamID)
		mu.Unlock()
	}
	f = started(t, fixtureOptions{sql: "UPDATE boss SET hp = 1", rules: func(r *teamstate.Rules) { r.OnFinished = hook }})
	n := f.bosses.Count()
	for round := range n {
		mu.Lock()
		got := len(calls)
		mu.Unlock()
		if got != 0 {
			t.Fatalf("finished after %d of %d bosses", round, n)
		}
		f.castAlternating(t, round)
	}
	mu.Lock()
	if len(calls) != 1 || calls[0] != f.teamID {
		t.Fatalf("OnFinished calls: %v, want one for %s", calls, f.teamID)
	}
	calls = nil
	mu.Unlock()

	f.restart(t) // the fixture's teams never mark it FINISHED: still IN_GAME
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != f.teamID {
		t.Fatalf("a restart finding it unfinished: %v", calls)
	}
}

// TestTeamDeleted checks that TeamDeleted stops the team's timer and forgets
// its state, and that the timer firing later recreates nothing.
func TestTeamDeleted(t *testing.T) {
	f := started(t, fixtureOptions{attacks: true})
	f.castQuad(t, 0)
	if f.clock.pending() != 1 {
		t.Fatalf("%d timers before the delete, want 1", f.clock.pending())
	}
	f.state.TeamDeleted(f.teamID)
	if f.clock.pending() != 0 {
		t.Fatalf("%d timers after the delete", f.clock.pending())
	}
	f.clock.at(24 * time.Hour)
	if st := f.state.ForTeam(f.teamID); st.Boss.Defeated != 0 || len(st.Casts) != 0 || f.clock.pending() != 0 {
		t.Fatalf("after the delete: %+v, %d timers", st, f.clock.pending())
	}
	for _, sd := range f.state.Standings() {
		if sd.TeamID == f.teamID {
			t.Fatal("a deleted team still has a standing")
		}
	}
}

// TestStandings checks Standings: a team's bosses defeated, when its last
// boss fell, and its damage on the current boss.
func TestStandings(t *testing.T) {
	f := started(t, fixtureOptions{sql: "UPDATE boss SET hp = 1 WHERE serial = 1"})
	res := f.castQuad(t, 0) // defeats boss 1 (1 HP)
	res2, dmg := f.castAlternating(t, 1)
	var got []teamstate.Standing
	for _, sd := range f.state.Standings() {
		if sd.TeamID == f.teamID {
			got = append(got, sd)
		}
	}
	want := teamstate.Standing{TeamID: f.teamID, BossesDefeated: 1, LastDefeatAt: res.State.LastCast.CastAt, Damage: dmg}
	if res2.State.BossesDefeated != 1 {
		t.Fatalf("the second cast (the triangle, damage %d) defeated boss 2 too: the test needs boss 2's HP above a triangle's damage", dmg)
	}
	if len(got) != 1 || got[0].BossesDefeated != want.BossesDefeated || !got[0].LastDefeatAt.Equal(want.LastDefeatAt) || got[0].Damage != want.Damage {
		t.Fatalf("standings %+v, want %+v", got, want)
	}
}

// staleTeams is a teamstate.Teams whose ForUser still answers with the team
// a user was removed from: the lookup a write makes before taking the team's
// lock, run just before an admin's removal applied.
type staleTeams struct {
	*teams.Store
	stale map[uuid.UUID]teams.Team
}

// ForUser returns the stale team for a removed user, else the real one.
func (s staleTeams) ForUser(id uuid.UUID) (teams.Team, error) {
	if t, ok := s.stale[id]; ok {
		return t, nil
	}
	return s.Store.ForUser(id)
}

// TestRemovedDuringWrite checks every member write's second membership check,
// under the team's lock: a member removed by an admin after the write looked
// their team up (staleTeams) is refused not_in_team, and nothing changes.
func TestRemovedDuringWrite(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	q := pgtest.Must(f.qtes.Create(ctx, f.users[0], qte.Draft{Kind: qte.KindQuiz, Powerup: "heart",
		Quiz: &qte.Quiz{Question: "Q", Options: []string{"A", "B"}, CorrectOption: 0}}, true))
	gone := f.users[1]
	before := pgtest.Must(f.teams.ForUser(gone))
	pgtest.Must(f.teams.RemoveMember(ctx, f.teamID, gone, nil))
	f.state.Close()
	f.state = pgtest.Must(teamstate.Load(ctx, pgtest.Open(t, f.dbURL), f.atlas, f.bosses,
		staleTeams{Store: f.teams, stale: map[uuid.UUID]teams.Team{gone: before}}, passQR{}, f.qtes, f.hub, pgtest.Quiet, f.rules))
	was := withoutVersionJSON(t, f.view(t))

	for name, write := range map[string]func() error{
		"select":       func() error { _, err := f.state.Select(ctx, gone, "quadrilateral"); return err },
		"clear":        func() error { _, err := f.state.ClearSelection(ctx, gone); return err },
		"scan":         func() error { _, err := f.state.Scan(ctx, gone, "2001"); return err },
		"activate":     func() error { _, err := f.state.Activate(ctx, gone, teamstate.PowerupHeart); return err },
		"instant cast": func() error { _, err := f.state.InstantCast(ctx, gone, quad); return err },
		"answer":       func() error { _, err := f.state.AnswerQTE(ctx, gone, q.ID, 0); return err },
	} {
		if err := write(); !errors.Is(err, teamstate.ErrNotInTeam) {
			t.Errorf("%s by a member removed meanwhile: %v", name, err)
		}
	}
	if now := withoutVersionJSON(t, f.view(t)); now != was {
		t.Fatalf("refused writes changed the state\nbefore: %s\nafter:  %s", was, now)
	}
}
