package tests

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/google/uuid"

	"labyrinth/internal/atlas"
	"labyrinth/internal/bosses"
	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
)

// benchPlayers is the game-day player count BenchmarkTeamStatePublish
// models: about 10,000 players, split into teams of teams.size.
const benchPlayers = 10_000

// memTeams is a teamstate.Teams of IN_GAME teams held in a map, so the
// benchmark needs no team rows in Postgres.
type memTeams map[string]teams.Team

// ForUser is unused by publishing.
func (m memTeams) ForUser(uuid.UUID) (teams.Team, error) { return teams.Team{}, teams.ErrNotInTeam }

// All returns every team, in no particular order.
func (m memTeams) All() []teams.Team {
	out := make([]teams.Team, 0, len(m))
	for _, t := range m {
		out = append(out, t)
	}
	return out
}

// ByID returns the team with that ID.
func (m memTeams) ByID(id string) (teams.Team, bool) {
	t, ok := m[id]
	return t, ok
}

// BenchmarkTeamStatePublish measures publishing team_state from many
// goroutines across the game-day number of teams, every member with an open
// stream: the team lock, encoding the state once, and queueing it for each
// member. Reports the number of teams alongside ns/op.
func BenchmarkTeamStatePublish(b *testing.B) {
	ctx := context.Background()
	set := configtest.Settings(b)
	db := pgtest.Open(b, pgtest.URL(b, "teamstate_test"))
	if err := db.SeedDev2025(ctx); err != nil {
		b.Fatal(err)
	}
	hub := live.NewHub(set.Live.HubShards, set.Live.SubscriberBuffer)
	defer hub.Close()

	ts := memTeams{}
	var ids []string
	for i := range benchPlayers / set.Teams.Size {
		t := teams.Team{ID: fmt.Sprintf("B%05d", i), Status: teams.InGame}
		for range set.Teams.Size {
			m := teams.Member{ID: uuid.New()}
			t.Members = append(t.Members, m)
			sub := hub.Subscribe(m.ID)
			go func() { // a client that keeps up
				for range sub.C {
				}
			}()
		}
		ts[t.ID] = t
		ids = append(ids, t.ID)
	}

	at := pgtest.Must(atlas.Load(ctx, db, pgtest.Quiet, atlas.Rules{MaxPatternPoints: set.Game.MaxPatternPoints}))
	bs := pgtest.Must(bosses.Load(ctx, db, pgtest.Quiet, bosses.Rules{Count: set.Game.Bosses}))
	st := pgtest.Must(teamstate.Load(ctx, db, at, bs, ts, hub, pgtest.Quiet, teamstate.Rules{
		MaxPatternPoints: set.Game.MaxPatternPoints,
		Health:           set.Game.Health,
		WriteTimeout:     set.Storage.WriteTimeout.Duration,
	}))
	for _, id := range ids { // warm: create each entry and cache its roster
		st.PublishTeam(id)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := rand.IntN(len(ids)) // each goroutine walks the teams from its own place
		for pb.Next() {
			st.PublishTeam(ids[i%len(ids)])
			i++
		}
	})
	b.ReportMetric(float64(len(ids)), "teams")
}
