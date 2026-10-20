// Command spells is an offline generator that reads waypoints and patterns from
// Postgres, computes calibrated damage values using internal/spells, and writes
// the seed SQL file backend/internal/platform/postgres/seeds/dev_2025_spells.sql.
//
// Usage, from backend/ like every other command, with the backend stopped:
//
//	go run ./cmd/spells
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"

	"labyrinth/internal/platform/config"
	"labyrinth/internal/spells"
)

// main runs the offline damage generator.
func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(context.Background(), log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// seedPath is the generated seed file, relative to backend/.
const seedPath = "internal/platform/postgres/seeds/dev_2025_spells.sql"

// devSeedPath is the source dev seed file containing waypoints and patterns, relative to backend/.
const devSeedPath = "internal/platform/postgres/seeds/dev_2025.sql"

// waypointPattern matches an INSERT into waypoint in dev_2025.sql.
var waypointPattern = regexp.MustCompile(`\('(\d{4})',\s*'[^']+',\s*([0-9.-]+),\s*([0-9.-]+)\)`)

// patternWaypointPattern matches an INSERT into pattern_waypoint in dev_2025.sql.
var patternWaypointPattern = regexp.MustCompile(`\((\d+),\s*'(\d{4})'\)`)

// patternRow holds a pattern's database ID and its waypoint IDs.
type patternRow struct {
	// id is pattern.id.
	id int
	// waypointIDs are the IDs of the waypoints forming this pattern.
	waypointIDs []string
}

// readFromSeed parses waypoints and patterns directly from dev_2025.sql when DATABASE_URL is not set.
func readFromSeed(path string) ([]spells.Waypoint, []patternRow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read dev seed: %w", err)
	}
	content := string(data)

	wpMatches := waypointPattern.FindAllStringSubmatch(content, -1)
	var waypoints []spells.Waypoint
	for _, m := range wpMatches {
		lat, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			return nil, nil, fmt.Errorf("parse lat %q: %w", m[2], err)
		}
		lon, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			return nil, nil, fmt.Errorf("parse lon %q: %w", m[3], err)
		}
		waypoints = append(waypoints, spells.Waypoint{
			ID:  m[1],
			GPS: spells.GPS{Latitude: lat, Longitude: lon},
		})
	}

	pwMatches := patternWaypointPattern.FindAllStringSubmatch(content, -1)
	patternMap := make(map[int][]string)
	for _, m := range pwMatches {
		pid, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, nil, fmt.Errorf("parse pid %q: %w", m[1], err)
		}
		patternMap[pid] = append(patternMap[pid], m[2])
	}

	var pids []int
	for pid := range patternMap {
		pids = append(pids, pid)
	}
	sort.Ints(pids)

	var patterns []patternRow
	for _, pid := range pids {
		wids := patternMap[pid]
		sort.Strings(wids)
		patterns = append(patterns, patternRow{
			id:          pid,
			waypointIDs: wids,
		})
	}

	return waypoints, patterns, nil
}

// run connects to Postgres (or falls back to dev_2025.sql), computes damage values, and writes dev_2025_spells.sql.
func run(ctx context.Context, log *slog.Logger) error {
	_ = godotenv.Load()

	if _, err := os.Stat(filepath.Dir(seedPath)); err != nil {
		return fmt.Errorf("run from backend/: %s not found", filepath.Dir(seedPath))
	}
	settings, err := config.LoadSettings(config.SettingsPath())
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	var (
		waypoints []spells.Waypoint
		patterns  []patternRow
	)

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL != "" {
		conn, err := pgx.Connect(ctx, dbURL)
		if err != nil {
			return fmt.Errorf("connect to database: %w", err)
		}
		defer conn.Close(ctx)

		rows, err := conn.Query(ctx, "SELECT id, latitude, longitude FROM waypoint ORDER BY id")
		if err != nil {
			return fmt.Errorf("query waypoints: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var w spells.Waypoint
			if err := rows.Scan(&w.ID, &w.GPS.Latitude, &w.GPS.Longitude); err != nil {
				return fmt.Errorf("scan waypoint: %w", err)
			}
			waypoints = append(waypoints, w)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate waypoints: %w", err)
		}

		pRows, err := conn.Query(ctx, `
			SELECT p.id, COALESCE(array_agg(pw.waypoint_id ORDER BY pw.waypoint_id)
			                      FILTER (WHERE pw.waypoint_id IS NOT NULL), '{}')
			FROM pattern p
			LEFT JOIN pattern_waypoint pw ON pw.pattern_id = p.id
			GROUP BY p.id
			ORDER BY p.id`)
		if err != nil {
			return fmt.Errorf("query patterns: %w", err)
		}
		defer pRows.Close()

		for pRows.Next() {
			var pr patternRow
			if err := pRows.Scan(&pr.id, &pr.waypointIDs); err != nil {
				return fmt.Errorf("scan pattern: %w", err)
			}
			patterns = append(patterns, pr)
		}
		if err := pRows.Err(); err != nil {
			return fmt.Errorf("iterate patterns: %w", err)
		}
	} else {
		log.Info("DATABASE_URL not set; reading waypoints and patterns from " + devSeedPath)
		var err error
		waypoints, patterns, err = readFromSeed(devSeedPath)
		if err != nil {
			return err
		}
	}

	if len(waypoints) == 0 {
		return fmt.Errorf("no waypoints found")
	}
	if len(patterns) == 0 {
		return fmt.Errorf("no patterns found")
	}

	projected := spells.ProjectWaypoints(waypoints)
	baseMap := settings.Spell.BaseDamage()

	var b strings.Builder
	b.WriteString("-- Generated by `go run ./cmd/spells`. Don't edit; re-run it instead.\n")

	for _, p := range patterns {
		pts := make([]spells.Point, len(p.waypointIDs))
		for i, wid := range p.waypointIDs {
			pt, ok := projected[wid]
			if !ok {
				return fmt.Errorf("pattern %d references unknown waypoint %q", p.id, wid)
			}
			pts[i] = pt
		}
		damage := spells.CalculateDamage(pts, baseMap, settings.Spell.Bonus.ConcaveQuad, settings.Spell.Per100m, settings.Spell.Walkability)
		b.WriteString(fmt.Sprintf("UPDATE pattern SET damage = %d WHERE id = %d;\n", damage, p.id))
	}

	if err := os.WriteFile(seedPath, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write seed file: %w", err)
	}

	log.Info("generated spells seed", "patterns", len(patterns), "output", seedPath)
	return nil
}
