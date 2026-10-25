package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"labyrinth/internal/platform/config"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/qr"
)

// The play scenario: every team plays the game loop for a while, with every
// player's event stream open. Each team's leader selects the biggest of the
// quadrilateral, triangle and line that the team has unlocked (game.unlocks:
// unlocked_points, from the state) and didn't just cast (the spell cooldown
// forbids two casts of one class in a row); its members scan the pattern's checkpoints with QR
// tokens signed the way a display signs them, choosing only checkpoints the
// team's QR cooldown leaves free (from the state each response returns);
// with too few free, the team waits for the next to open. A knocked-out or
// finished team stops.

// seedContent migrates the bench database and loads the 2026 content into
// it: the play scenario needs waypoints and patterns, read by the backend at
// startup, so it runs before the backend starts.
func seedContent(ctx context.Context, dbURL string) error {
	db, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		return err
	}
	defer db.Close()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, quiet); err != nil {
		return err
	}
	return db.Seed2026(ctx)
}

// playWaypoints returns the IDs of every waypoint in at least one pattern,
// sorted.
func playWaypoints(ctx context.Context, dbURL string) ([]string, error) {
	db, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Pool().Query(ctx, "SELECT DISTINCT waypoint_id FROM pattern_waypoint ORDER BY waypoint_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// knownWaypoints is a qr.Atlas over a fixed list of waypoint IDs.
type knownWaypoints []string

// WaypointIndex finds id in the list.
func (k knownWaypoints) WaypointIndex(id string) (int, bool) {
	i := slices.Index(k, id)
	return i, i >= 0
}

// newSigner returns a function that signs a fresh QR token for a waypoint, as
// a display does: the per-waypoint key (from the qr package, with the
// backend's QR_SECRET) over "<waypoint>:<unix seconds>", base64url.
func newSigner(secret []byte, waypoints []string, rules qr.Rules) (func(string) string, error) {
	v := qr.New(secret, rules, knownWaypoints(waypoints))
	keys := map[string][]byte{}
	for _, id := range waypoints {
		sk, err := v.SigningKey(id)
		if err != nil {
			return nil, err
		}
		if keys[id], err = hex.DecodeString(sk.Key); err != nil {
			return nil, err
		}
	}
	return func(id string) string {
		payload := id + ":" + strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, keys[id])
		mac.Write([]byte(payload))
		return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}, nil
}

// playState is the part of a team's state the play loop needs, from a
// select or scan response.
type playState struct {
	QRCooldowns []struct {
		WaypointID string    `json:"waypoint_id"`
		Until      time.Time `json:"until"`
	} `json:"qr_cooldowns"`
	KnockedOut     bool `json:"knocked_out"`
	BossesDefeated int  `json:"bosses_defeated"`
	UnlockedPoints int  `json:"unlocked_points"`
}

// playShapes are the shapes the play scenario casts, biggest first, with
// their waypoint counts.
var playShapes = []struct {
	name   string
	points int
}{{"quadrilateral", 4}, {"triangle", 3}, {"line", 2}}

// playStats counts what the play scenario did.
type playStats struct {
	casts, waits, knockedOut, finished, bossesDefeated atomic.Int64
}

// play runs every team's game loop for d, members scanning with sign's
// tokens among waypoints, and returns what happened.
func play(c *client, teams []team, waypoints []string, sign func(string) string, d time.Duration) *playStats {
	stats := &playStats{}
	deadline := time.Now().Add(d)
	var wg sync.WaitGroup
	for _, t := range teams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			playTeam(c, t, waypoints, sign, deadline, stats)
		}()
	}
	wg.Wait()
	return stats
}

// playTeam is one team's loop until deadline: cast alternately a random
// free triangle and quadrilateral, or wait for checkpoints to open.
func playTeam(c *client, t team, waypoints []string, sign func(string) string, deadline time.Time, stats *playStats) {
	players := append([]player{t.leader}, t.members...)
	var st playState
	if env, ok := c.call("state", "GET", "/api/teams/me/state", t.leader, nil); !ok || json.Unmarshal(env.Payload, &st) != nil {
		return
	}
	defeated := 0
	last := "" // the shape of the team's last cast: its class is on cooldown
	for time.Now().Before(deadline) {
		shape, k := "", 0
		for _, s := range playShapes {
			if s.points <= st.UnlockedPoints && s.name != last {
				shape, k = s.name, s.points
				break
			}
		}
		if shape == "" { // can't happen with the 2026 content: lines are always open
			return
		}
		free := slices.Clone(waypoints)
		earliest := time.Time{}
		for _, cd := range st.QRCooldowns {
			if time.Now().Before(cd.Until) {
				free = slices.DeleteFunc(free, func(w string) bool { return w == cd.WaypointID })
				if earliest.IsZero() || cd.Until.Before(earliest) {
					earliest = cd.Until
				}
			}
		}
		if len(free) < k {
			stats.waits.Add(1)
			time.Sleep(min(time.Until(earliest)+50*time.Millisecond, time.Until(deadline), 5*time.Second))
			continue
		}
		env, ok := c.call("select", "PUT", "/api/teams/me/state/selection", t.leader, map[string]string{"shape": shape})
		switch {
		case env.Code == "knocked_out":
			stats.knockedOut.Add(1)
			return
		case env.Code == "not_in_game":
			stats.finished.Add(1)
			return
		case !ok:
			time.Sleep(time.Second)
			continue
		}
		rand.Shuffle(len(free), func(i, j int) { free[i], free[j] = free[j], free[i] })
		cast := true
		for i, w := range free[:k] {
			env, ok := c.call("scan", "POST", "/api/teams/me/state/scans", players[i%len(players)], map[string]string{"qr": sign(w)})
			if !ok {
				cast = false
				break
			}
			var res struct {
				Outcome string    `json:"outcome"`
				State   playState `json:"state"`
			}
			if json.Unmarshal(env.Payload, &res) == nil {
				st = res.State
			}
		}
		if cast {
			stats.casts.Add(1)
			last = shape
			if st.BossesDefeated > defeated {
				stats.bossesDefeated.Add(int64(st.BossesDefeated - defeated))
				defeated = st.BossesDefeated
			}
		}
	}
}

// override is one config.toml line to replace for the backend under test:
// the line "<key> = …" (key unique in the file) becomes "<key> = <value>",
// value written as TOML.
type override struct{ key, value string }

// configWith writes a copy of the config file at path with each override
// applied, and returns the copy's path: the backend under test reads it
// through CONFIG_FILE.
func configWith(path string, overrides ...override) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, o := range overrides {
		line := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(o.key) + ` = .*$`)
		if n := len(line.FindAll(data, -1)); n != 1 {
			return "", fmt.Errorf("%s has %d %q lines to override, want 1", path, n, o.key)
		}
		data = line.ReplaceAll(data, []byte(o.key+" = "+o.value))
	}
	tmp := filepath.Join(os.TempDir(), "labyrinth-loadtest-config.toml")
	return tmp, os.WriteFile(tmp, data, 0o600)
}

// loadQRSecret reads QR_SECRET (from the environment or .env, already
// loaded), as the backend does.
func loadQRSecret() ([]byte, error) {
	secret, err := config.ParseQRSecret(os.Getenv("QR_SECRET"))
	if err != nil {
		return nil, fmt.Errorf("QR_SECRET: %w", err)
	}
	return secret, nil
}
