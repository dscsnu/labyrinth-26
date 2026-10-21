package tests

import (
	"context"
	"strings"
	"testing"

	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/qte"
	"labyrinth/internal/teamstate"
)

// TestLoadReadErrors checks that Load fails, rather than starting with part
// of the state, when any of its reads fails, in each of the two ways a read
// can: the query itself (a table or column missing: refused when it's
// prepared) and a row (a view standing in for the table that errors while
// rows stream: division by zero, volatile so it can't fail at planning).
// The team has a row in every table first: a cast, a held powerup, a quiz
// answer, a selection and a scan after it.
func TestLoadReadErrors(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	f.castQuad(t, 0)
	f.grant(t, teamstate.PowerupHeart)
	q := pgtest.Must(f.qtes.Create(ctx, f.users[0], qte.Draft{Kind: qte.KindQuiz, Powerup: "heart",
		Quiz: &qte.Quiz{Question: "Q", Options: []string{"A", "B"}, CorrectOption: 0}}, true))
	f.answer(t, 0, q, 1)
	f.selectAt(t, f.tick(500), 0, "triangle")
	f.scanAt(t, f.tick(501), 0, "2003")

	exec := func(sql string) {
		t.Helper()
		if _, err := f.db.Pool().Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// missing renames table to table_real; runtime puts a view over it that
	// errors on its first row.
	missing := func(table string) (string, string) {
		return "ALTER TABLE " + table + " RENAME TO " + table + "_real",
			"ALTER TABLE " + table + "_real RENAME TO " + table
	}
	runtime := func(table string) (string, string) {
		return "ALTER TABLE " + table + " RENAME TO " + table + "_real; CREATE VIEW " + table +
				" AS SELECT * FROM " + table + "_real WHERE 1 / (random() * 0)::int = 1",
			"DROP VIEW " + table + "; ALTER TABLE " + table + "_real RENAME TO " + table
	}
	type breakage struct {
		name        string
		break_, fix string
	}
	var cases []breakage
	for _, table := range []string{"team_scan", "team_pattern", "team_cast", "team_boss", "team_qte_answer", "team_powerup", "team_selection"} {
		b, fx := missing(table)
		cases = append(cases, breakage{table + " missing", b, fx})
		b, fx = runtime(table)
		cases = append(cases, breakage{table + " failing a row", b, fx})
	}
	// The stack query reads team_scan's columns the visited-waypoints query
	// doesn't: break only those.
	cases = append(cases,
		breakage{"stack: team_scan.wrong missing",
			"ALTER TABLE team_scan RENAME COLUMN wrong TO wrong_real",
			"ALTER TABLE team_scan RENAME COLUMN wrong_real TO wrong"},
		breakage{"stack: team_scan.wrong failing a row",
			"ALTER TABLE team_scan RENAME TO team_scan_real; CREATE VIEW team_scan AS SELECT id, team_id, waypoint_id, user_id, scanned_at, " +
				"(1 / (random() * 0)::int = 1) AS wrong FROM team_scan_real",
			"DROP VIEW team_scan; ALTER TABLE team_scan_real RENAME TO team_scan"})

	for _, c := range cases {
		exec(c.break_)
		_, err := teamstate.Load(ctx, pgtest.Open(t, f.dbURL), f.atlas, f.bosses, f.teams, passQR{}, f.qtes, f.hub, pgtest.Quiet, f.rules)
		exec(c.fix)
		if err == nil || !strings.HasPrefix(err.Error(), "load team state: ") {
			t.Errorf("%s: load: %v", c.name, err)
		}
	}
	// Mended, it loads the same state.
	before := withoutVersionJSON(t, f.view(t))
	f.restart(t)
	if after := withoutVersionJSON(t, f.view(t)); after != before {
		t.Fatalf("after mending\nbefore: %s\nafter:  %s", before, after)
	}
}
