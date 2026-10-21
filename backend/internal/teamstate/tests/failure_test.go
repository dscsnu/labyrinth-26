package tests

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/atlas"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
)

// failWrites makes every insert, update and delete on table fail in Postgres
// (a trigger raising), so a write-through fails at exactly that step while
// memory is untouched; the returned func removes it.
func (f *fixtureEnv) failWrites(t *testing.T, table string) func() {
	t.Helper()
	ctx := context.Background()
	if _, err := f.db.Pool().Exec(ctx, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION planted_failure() RETURNS trigger LANGUAGE plpgsql AS
			$$ BEGIN RAISE EXCEPTION 'planted failure on %%', TG_TABLE_NAME; END $$;
		CREATE TRIGGER planted_failure BEFORE INSERT OR UPDATE OR DELETE ON %s
			FOR EACH ROW EXECUTE FUNCTION planted_failure()`, table)); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := f.db.Pool().Exec(ctx, "DROP TRIGGER planted_failure ON "+table); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPersistFailures checks every write whose write-through can fail, at
// each step of the multi-row ones: Postgres refuses (failWrites), the write
// returns the error (not a rule's), the members get resync with the state
// as it was (memory reloaded, Postgres wins), and a restart agrees.
func TestPersistFailures(t *testing.T) {
	ctx := context.Background()
	// last scans the quadrilateral's first three points: its fourth casts.
	last := func(f *fixtureEnv) {
		pgtest.Must(f.state.Select(ctx, f.users[0], "quadrilateral"))
		for _, w := range quad[:3] {
			pgtest.Must(f.state.Scan(ctx, f.users[0], w))
		}
	}
	castLast := func(f *fixtureEnv) error { _, err := f.state.Scan(ctx, f.users[0], quad[3]); return err }
	for _, c := range []struct {
		name  string
		opts  fixtureOptions
		setup func(f *fixtureEnv)
		table string
		write func(f *fixtureEnv) error
	}{
		{name: "select, nothing stored yet", table: "team_selection",
			write: func(f *fixtureEnv) error { _, err := f.state.Select(ctx, f.users[0], "quadrilateral"); return err }},
		{name: "clear", table: "team_selection",
			setup: func(f *fixtureEnv) { pgtest.Must(f.state.Select(ctx, f.users[0], "quadrilateral")) },
			write: func(f *fixtureEnv) error { _, err := f.state.ClearSelection(ctx, f.users[0]); return err }},
		{name: "cast: team_scan", setup: last, table: "team_scan", write: castLast},
		{name: "cast: team_pattern", setup: last, table: "team_pattern", write: castLast},
		{name: "cast: team_cast", setup: last, table: "team_cast", write: castLast},
		{name: "cast: team_boss", setup: last, table: "team_boss", write: castLast},
		{name: "revive", table: "team_boss",
			write: func(f *fixtureEnv) error { _, err := f.state.Revive(ctx, f.teamID); return err }},
		{name: "reset cooldowns", table: "team_boss",
			setup: func(f *fixtureEnv) { f.castQuad(t, 0) },
			write: func(f *fixtureEnv) error { _, err := f.state.ResetCooldowns(ctx, f.teamID); return err }},
		{name: "grant", table: "team_powerup",
			write: func(f *fixtureEnv) error {
				_, err := f.state.GrantPowerup(ctx, f.teamID, teamstate.PowerupHeart)
				return err
			}},
		{name: "heart: team_powerup", opts: fixtureOptions{attacks: true}, table: "team_powerup",
			setup: func(f *fixtureEnv) { f.clock.at(f.interval(1)); f.grant(t, teamstate.PowerupHeart) },
			write: func(f *fixtureEnv) error {
				_, err := f.state.Activate(ctx, f.users[0], teamstate.PowerupHeart)
				return err
			}},
		{name: "heart: team_boss", opts: fixtureOptions{attacks: true}, table: "team_boss",
			setup: func(f *fixtureEnv) { f.clock.at(f.interval(1)); f.grant(t, teamstate.PowerupHeart) },
			write: func(f *fixtureEnv) error {
				_, err := f.state.Activate(ctx, f.users[0], teamstate.PowerupHeart)
				return err
			}},
		{name: "instant cast: team_powerup", table: "team_powerup",
			setup: func(f *fixtureEnv) { f.grant(t, teamstate.PowerupInstantCast) },
			write: func(f *fixtureEnv) error { _, err := f.state.InstantCast(ctx, f.users[0], quad); return err }},
		{name: "instant cast: team_cast", table: "team_cast",
			setup: func(f *fixtureEnv) { f.grant(t, teamstate.PowerupInstantCast) },
			write: func(f *fixtureEnv) error { _, err := f.state.InstantCast(ctx, f.users[0], quad); return err }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := started(t, c.opts)
			if c.setup != nil {
				c.setup(f)
			}
			before := withoutVersionJSON(t, f.view(t))
			sub := f.hub.Subscribe(f.users[1])
			defer sub.Close()
			undo := f.failWrites(t, c.table)
			err := c.write(f)
			undo()
			if err == nil || !strings.Contains(err.Error(), "planted failure on "+c.table) {
				t.Fatalf("the write: %v", err)
			}
			if v := expectType(t, sub, "reconcile", live.Resync); withoutVersionJSON(t, v) != before {
				t.Fatalf("after the reconcile\nbefore: %s\nafter:  %s", before, withoutVersionJSON(t, v))
			}
			f.restart(t)
			if after := withoutVersionJSON(t, f.view(t)); after != before {
				t.Fatalf("Postgres after the failed write\nbefore: %s\nafter:  %s", before, after)
			}
		})
	}
}

// brokenQR is a teamstate.QR whose verifier fails with an error of its own
// (neither invalid nor expired).
type brokenQR struct{}

// errVerifier is what brokenQR fails with.
var errVerifier = errors.New("verifier down")

// Verify fails.
func (brokenQR) Verify(string) (string, error) { return "", errVerifier }

// TestScanVerifierError checks that a verifier error other than invalid or
// expired is returned as it is (the route makes it a 500), nothing changed.
func TestScanVerifierError(t *testing.T) {
	f := started(t)
	f.state.Close()
	f.state = pgtest.Must(teamstate.Load(context.Background(), pgtest.Open(t, f.dbURL), f.atlas, f.bosses, f.teams, brokenQR{}, f.qtes, f.hub, pgtest.Quiet, f.rules))
	if _, err := f.state.Scan(context.Background(), f.users[0], "2001"); !errors.Is(err, errVerifier) {
		t.Fatalf("scan with the verifier down: %v", err)
	}
}

// TestInstantCastNoTeam checks InstantCast by someone in no team.
func TestInstantCastNoTeam(t *testing.T) {
	f := started(t)
	if _, err := f.state.InstantCast(context.Background(), uuid.New(), quad); !errors.Is(err, teamstate.ErrNotInTeam) {
		t.Fatalf("instant cast with no team: %v", err)
	}
}

// hidingAtlas is the real atlas missing one piece of content, as if it had
// been removed between restarts: SetOf refuses setWaypoint, WaypointIndex
// refuses waypoint, ShapeIndex refuses shape, PatternByID refuses pattern.
type hidingAtlas struct {
	*atlas.Atlas
	setWaypoint, waypoint, shape string
	pattern                      int
}

// SetOf refuses a set naming setWaypoint.
func (h hidingAtlas) SetOf(ids []string) (atlas.WaypointSet, error) {
	if slices.Contains(ids, h.setWaypoint) {
		var none atlas.WaypointSet
		return none, fmt.Errorf("unknown waypoint %q", h.setWaypoint)
	}
	return h.Atlas.SetOf(ids)
}

// WaypointIndex refuses waypoint.
func (h hidingAtlas) WaypointIndex(id string) (int, bool) {
	if id == h.waypoint {
		return 0, false
	}
	return h.Atlas.WaypointIndex(id)
}

// ShapeIndex refuses shape.
func (h hidingAtlas) ShapeIndex(id string) (int, bool) {
	if id == h.shape {
		return 0, false
	}
	return h.Atlas.ShapeIndex(id)
}

// PatternByID refuses pattern.
func (h hidingAtlas) PatternByID(id int) (int, bool) {
	if id == h.pattern {
		return 0, false
	}
	return h.Atlas.PatternByID(id)
}

// TestLoadRefusesUnknownContent checks that Load refuses stored state naming
// content the atlas no longer has (content changed between restarts):
// a visited waypoint, a completed pattern, the selected shape, a cast's
// pattern (with no team_pattern row naming it), a stack scan's waypoint.
func TestLoadRefusesUnknownContent(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	f.castQuad(t, 0)
	f.selectAt(t, f.tick(500), 0, "triangle")
	f.scanAt(t, f.tick(501), 0, "2003")
	p, _ := f.atlas.PatternFor(pgtest.Must(f.atlas.SetOf(quad)))
	qd := f.atlas.Pattern(p).ID
	for _, c := range []struct {
		name, sql string
		hide      hidingAtlas
		want      string
	}{
		{name: "visited waypoint", hide: hidingAtlas{setWaypoint: "2007"}, want: `unknown waypoint "2007"`},
		{name: "completed pattern", hide: hidingAtlas{pattern: qd}, want: fmt.Sprintf("unknown pattern %d", qd)},
		{name: "selected shape", hide: hidingAtlas{shape: "triangle"}, want: `selection: unknown shape "triangle"`},
		{name: "stack scan", hide: hidingAtlas{waypoint: "2003"}, want: `stack scan: unknown waypoint "2003"`},
		{name: "cast's pattern", sql: "DELETE FROM team_pattern", hide: hidingAtlas{pattern: qd}, want: fmt.Sprintf("cast: unknown pattern %d", qd)},
	} {
		if c.sql != "" {
			if _, err := f.db.Pool().Exec(ctx, c.sql); err != nil {
				t.Fatal(err)
			}
		}
		c.hide.Atlas = f.atlas
		_, err := teamstate.Load(ctx, pgtest.Open(t, f.dbURL), c.hide, f.bosses, f.teams, passQR{}, f.qtes, f.hub, pgtest.Quiet, f.rules)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: load: %v, want %q", c.name, err, c.want)
		}
	}
}

// noTeamState fails if a team_state event is waiting on sub (other events,
// a no_team, are skipped).
func noTeamState(t *testing.T, sub *live.Subscription, what string) {
	t.Helper()
	for {
		select {
		case ev := <-sub.C:
			if ev.Name() == "team_state" {
				t.Fatalf("%s: unexpected team_state %s", what, ev.Data())
			}
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

// TestTimerAfterTeamDeleted checks a team's timer firing after TeamDeleted
// (the stop came too late: the fake clock's leaky): it finds no entry and
// recreates nothing.
func TestTimerAfterTeamDeleted(t *testing.T) {
	f := started(t, fixtureOptions{attacks: true})
	f.clock.leaky = true
	sub := f.hub.Subscribe(f.users[0])
	defer sub.Close()
	f.state.TeamDeleted(f.teamID)
	f.clock.at(f.interval(1))
	noTeamState(t, sub, "a timer after the delete")
	for _, sd := range f.state.Standings() {
		if sd.TeamID == f.teamID {
			t.Fatal("the late timer recreated the team")
		}
	}
	f.state.TeamDeleted(f.teamID) // again, and for a team it never had: no-ops
	f.state.TeamDeleted("000000")
}

// TestTimerAfterClose checks a team's timer firing after Close (leaky): the
// entry is closed, so nothing is applied or published.
func TestTimerAfterClose(t *testing.T) {
	f := started(t, fixtureOptions{attacks: true})
	f.clock.leaky = true
	sub := f.hub.Subscribe(f.users[0])
	defer sub.Close()
	f.state.Close()
	f.clock.at(f.interval(1))
	noTeamState(t, sub, "a timer after Close")
	if st := f.state.ForTeam(f.teamID); st.Boss.Attacks != 0 {
		t.Fatalf("a closed store applied an attack: %+v", st.Boss)
	}
}

// goneTeams is the teams store with one team already gone from ByID (an
// admin's deletion applied in teams, its OnDelete hook not yet run), still
// listed by All so Load arms its timer.
type goneTeams struct {
	*teams.Store
	gone string
}

// ByID hides the gone team.
func (g goneTeams) ByID(id string) (teams.Team, bool) {
	if id == g.gone {
		return teams.Team{}, false
	}
	return g.Store.ByID(id)
}

// TestPublishTeamGone checks a publish for a team teams no longer has (the
// timer applying an attack in the moment between teams' deletion and its
// hook): the change applies in memory, and is published to nobody.
func TestPublishTeamGone(t *testing.T) {
	f := started(t, fixtureOptions{attacks: true})
	f.state.Close()
	f.state = pgtest.Must(teamstate.Load(context.Background(), pgtest.Open(t, f.dbURL), f.atlas, f.bosses,
		goneTeams{Store: f.teams, gone: f.teamID}, passQR{}, f.qtes, f.hub, pgtest.Quiet, f.rules))
	sub := f.hub.Subscribe(f.users[0])
	defer sub.Close()
	f.clock.at(f.interval(1))
	noTeamState(t, sub, "an attack on a gone team")
	if st := f.state.ForTeam(f.teamID); st.Boss.Attacks != 1 {
		t.Fatalf("the attack wasn't applied: %+v", st.Boss)
	}
}
