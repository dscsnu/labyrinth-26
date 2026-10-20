package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"labyrinth/internal/platform/config"
)

// validTOML is a complete, valid config.toml.
const validTOML = `
[teams]
size = 6
name_max_length = 32
code_attempts = 10
[game]
max_pattern_points = 6
bosses = 6
health = 10
locked_pattern_points = 5
unlock_after_bosses = 5
[game.boss_attack]
interval = "10m"
[game.boss_attack.damage]
1 = 1
2 = 2
3 = 3
4 = 4
5 = 5
6 = 6
[qr]
rotate = "5s"
tolerance = "5s"
[spell]
walkability = 1.5
per_100m = 0.8
[spell.base]
1 = 15
2 = 18
3 = 52
4 = 179
5 = 376
6 = 1174
[spell.bonus]
concave_quad = 75
[auth]
token_ttl = "12h"
login_code_ttl = "2m"
oauth_state_ttl = "10m"
login_code_sweep = "10m"
[live]
heartbeat = "25s"
subscriber_buffer = 16
hub_shards = 64
write_timeout = "10s"
retry_min = "1s"
retry_max = "6s"
[server]
read_header_timeout = "10s"
read_timeout = "30s"
idle_timeout = "2m"
shutdown_timeout = "10s"
max_body_bytes = 1048576
[storage]
write_timeout = "5s"
instance_lock_check = "5s"
`

// writeTOML writes content to a temporary config.toml and returns its path.
func writeTOML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRepositoryConfigLoads checks that the committed backend/config.toml is
// valid.
func TestRepositoryConfigLoads(t *testing.T) {
	if _, err := config.LoadSettings(filepath.Join("..", "..", "..", "..", "config.toml")); err != nil {
		t.Fatal(err)
	}
}

// TestLoadSettings checks that a valid file decodes every value.
func TestLoadSettings(t *testing.T) {
	s, err := config.LoadSettings(writeTOML(t, validTOML))
	if err != nil {
		t.Fatal(err)
	}
	if s.Teams.Size != 6 || s.Game.MaxPatternPoints != 6 || s.Game.Bosses != 6 || s.Game.Health != 10 || s.Game.LockedPatternPoints != 5 || s.Game.UnlockAfterBosses != 5 ||
		s.Game.BossAttack.Interval.Duration != 10*time.Minute || s.Game.BossAttack.BossDamage()[6] != 6 || len(s.Game.BossAttack.Damage) != 6 ||
		s.QR.Rotate.Duration != 5*time.Second || s.QR.Tolerance.Duration != 5*time.Second ||
		s.Spell.Walkability != 1.5 || s.Spell.Per100m != 0.8 || s.Spell.Bonus.ConcaveQuad != 75 || s.Spell.BaseDamage()[4] != 179 || len(s.Spell.Base) != 6 ||
		s.Auth.TokenTTL.Duration != 12*time.Hour || s.Live.SubscriberBuffer != 16 || s.Storage.WriteTimeout.Duration != 5*time.Second {
		t.Fatalf("decoded %+v", s)
	}
}

// TestLoadSettingsRejects checks every kind of bad file is refused, naming
// the problem: unknown and missing keys, a bad duration, and every value
// validate can reject.
func TestLoadSettingsRejects(t *testing.T) {
	cases := map[string]struct{ from, to, wantInErr string }{
		"unknown key":                         {"size = 6", "size = 6\nsizee = 7", "unknown key"},
		"missing value":                       {"heartbeat = \"25s\"\n", "", "live.heartbeat"},
		"bad duration":                        {`heartbeat = "25s"`, `heartbeat = "a while"`, "heartbeat"},
		"missing per_100m (zero is valid)":    {"per_100m = 0.8\n", "", "missing key spell.per_100m"},
		"state ttl under a second":            {`oauth_state_ttl = "10m"`, `oauth_state_ttl = "500ms"`, "oauth_state_ttl"},
		"read timeout under header timeout":   {`read_timeout = "30s"`, `read_timeout = "1s"`, "read_timeout"},
		"zero body limit":                     {"max_body_bytes = 1048576", "max_body_bytes = 0", "max_body_bytes"},
		"team too big":                        {"size = 6", "size = 17", "teams.size"},
		"name longer than the database":       {"name_max_length = 32", "name_max_length = 33", "name_max_length"},
		"pattern bigger than the stack":       {"max_pattern_points = 6", "max_pattern_points = 16", "max_pattern_points"},
		"no bosses":                           {"bosses = 6", "bosses = 0", "game.bosses must be at least 1"},
		"missing attack interval":             {"interval = \"10m\"\n", "", "missing key game.boss_attack.interval"},
		"zero attack interval":                {`interval = "10m"`, `interval = "0s"`, "game.boss_attack.interval must be positive"},
		"no damage for a boss":                {"6 = 6\n", "", "game.boss_attack.damage has no entry for boss 6"},
		"damage for a boss that isn't":        {"6 = 6\n", "6 = 6\n7 = 7\n", "game.boss_attack.damage.7 is not a boss serial (1-6, game.bosses)"},
		"damage key not a serial":             {"6 = 6\n", "6 = 6\nboss = 1\n", "game.boss_attack.damage.boss is not a boss serial"},
		"negative damage":                     {"3 = 3", "3 = -1", "game.boss_attack.damage.3 must not be negative"},
		"locked size past the stack":          {"locked_pattern_points = 5", "locked_pattern_points = 8", "game.locked_pattern_points must be 1-7"},
		"locked size zero":                    {"locked_pattern_points = 5", "locked_pattern_points = 0", "game.locked_pattern_points must be 1-7"},
		"unlock after more bosses than exist": {"unlock_after_bosses = 5", "unlock_after_bosses = 7", "game.unlock_after_bosses must be 0-6"},
		"negative unlock":                     {"unlock_after_bosses = 5", "unlock_after_bosses = -1", "game.unlock_after_bosses must be 0-6"},
		"no health":                           {"health = 10", "health = 0", "game.health must be at least 1"},
		"zero qr rotate":                      {`rotate = "5s"`, `rotate = "0s"`, "qr.rotate must be positive"},
		"zero qr tolerance":                   {`tolerance = "5s"`, `tolerance = "0s"`, "qr.tolerance must be positive"},
		"negative per_100m":                   {"per_100m = 0.8", "per_100m = -1", "spell.per_100m must not be negative"},
		"walkability under 1.0":               {"walkability = 1.5", "walkability = 0.5", "spell.walkability must be at least 1.0"},
		"no base damage for size":             {"6 = 1174\n", "", "spell.base has no entry for size 6"},
		"base damage for size that isn't":     {"6 = 1174\n", "6 = 1174\n7 = 2000\n", "spell.base.7 is not a valid pattern size (1-6, game.max_pattern_points)"},
		"base damage key not a size":          {"6 = 1174\n", "6 = 1174\nsize = 100\n", "spell.base.size is not a valid pattern size"},
		"zero base damage":                    {"2 = 18", "2 = 0", "spell.base.2 must be positive"},
		"negative concave_quad bonus":         {"concave_quad = 75", "concave_quad = -1", "spell.bonus.concave_quad must not be negative"},
		"zero code attempts":                  {"code_attempts = 10", "code_attempts = 0", "teams.code_attempts must be at least 1"},
		"zero token ttl":                      {`token_ttl = "12h"`, `token_ttl = "0s"`, "auth.token_ttl must be positive"},
		"zero login code ttl":                 {`login_code_ttl = "2m"`, `login_code_ttl = "0s"`, "auth.login_code_ttl must be positive"},
		"zero login code sweep":               {`login_code_sweep = "10m"`, `login_code_sweep = "0s"`, "auth.login_code_sweep must be positive"},
		"zero heartbeat":                      {`heartbeat = "25s"`, `heartbeat = "0s"`, "live.heartbeat must be positive"},
		"zero subscriber buffer":              {"subscriber_buffer = 16", "subscriber_buffer = 0", "live.subscriber_buffer must be at least 1"},
		"zero hub shards":                     {"hub_shards = 64", "hub_shards = 0", "live.hub_shards must be at least 1"},
		"zero live write timeout":             {`write_timeout = "10s"`, `write_timeout = "0s"`, "live.write_timeout must be positive"},
		"zero retry min":                      {`retry_min = "1s"`, `retry_min = "0s"`, "live.retry_min must be at least 1ms"},
		"retry max below min":                 {`retry_max = "6s"`, `retry_max = "500ms"`, "live.retry_max must be at least live.retry_min"},
		"zero header timeout":                 {`read_header_timeout = "10s"`, `read_header_timeout = "0s"`, "server.read_header_timeout must be positive"},
		"zero idle timeout":                   {`idle_timeout = "2m"`, `idle_timeout = "0s"`, "server.idle_timeout must be positive"},
		"zero shutdown timeout":               {`shutdown_timeout = "10s"`, `shutdown_timeout = "0s"`, "server.shutdown_timeout must be positive"},
		"zero write timeout":                  {`write_timeout = "5s"`, `write_timeout = "0s"`, "storage.write_timeout must be positive"},
		"zero instance lock check":            {`instance_lock_check = "5s"`, `instance_lock_check = "0s"`, "storage.instance_lock_check must be positive"},
	}
	for name, c := range cases {
		if strings.Count(validTOML, c.from) != 1 {
			t.Fatalf("%s: %q must appear exactly once in validTOML", name, c.from)
		}
		content := strings.Replace(validTOML, c.from, c.to, 1)
		_, err := config.LoadSettings(writeTOML(t, content))
		if err == nil || !strings.Contains(err.Error(), c.wantInErr) {
			t.Errorf("%s: got %v, want an error mentioning %q", name, err, c.wantInErr)
		}
	}
}
