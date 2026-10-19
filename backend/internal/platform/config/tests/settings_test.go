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
scan_window = "1m"
scan_grace = "5s"
max_pattern_points = 6
[spell]
per_point = 15
per_100m = 2
walkability = 1.5
[auth]
token_ttl = "12h"
login_code_ttl = "2m"
oauth_state_ttl = "10m"
login_code_sweep = "10m"
[live]
heartbeat = "25s"
subscriber_buffer = 16
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
	if s.Teams.Size != 6 || s.Game.ScanWindow.Duration != time.Minute || s.Game.ScanGrace.Duration != 5*time.Second || s.Game.MaxPatternPoints != 6 ||
		s.Spell.PerPoint != 15 || s.Spell.Per100m != 2 || s.Spell.Walkability != 1.5 ||
		s.Auth.TokenTTL.Duration != 12*time.Hour || s.Live.SubscriberBuffer != 16 || s.Storage.WriteTimeout.Duration != 5*time.Second {
		t.Fatalf("decoded %+v", s)
	}
}

// TestLoadSettingsRejects checks every kind of bad file is refused, naming
// the problem: unknown and missing keys, a bad duration, and every value
// validate can reject.
func TestLoadSettingsRejects(t *testing.T) {
	cases := map[string]struct{ from, to, wantInErr string }{
		"unknown key":                       {"size = 6", "size = 6\nsizee = 7", "unknown key"},
		"missing value":                     {"heartbeat = \"25s\"\n", "", "live.heartbeat"},
		"bad duration":                      {`"1m"`, `"a minute"`, "scan_window"},
		"negative grace":                    {`scan_grace = "5s"`, `scan_grace = "-5s"`, "scan_grace"},
		"missing grace (zero is valid)":     {"scan_grace = \"5s\"\n", "", "missing key game.scan_grace"},
		"state ttl under a second":          {`oauth_state_ttl = "10m"`, `oauth_state_ttl = "500ms"`, "oauth_state_ttl"},
		"read timeout under header timeout": {`read_timeout = "30s"`, `read_timeout = "1s"`, "read_timeout"},
		"zero body limit":                   {"max_body_bytes = 1048576", "max_body_bytes = 0", "max_body_bytes"},
		"team too big":                      {"size = 6", "size = 17", "teams.size"},
		"name longer than the database":     {"name_max_length = 32", "name_max_length = 33", "name_max_length"},
		"more pattern points than members":  {"max_pattern_points = 6", "max_pattern_points = 7", "max_pattern_points"},
		"zero per_point":                    {"per_point = 15", "per_point = 0", "spell.per_point must be positive"},
		"negative per_100m":                 {"per_100m = 2", "per_100m = -1", "spell.per_100m must not be negative"},
		"walkability under 1.0":             {"walkability = 1.5", "walkability = 0.5", "spell.walkability must be at least 1.0"},
		"zero window":                       {`scan_window = "1m"`, `scan_window = "0s"`, "scan_window"},
		"zero code attempts":                {"code_attempts = 10", "code_attempts = 0", "teams.code_attempts must be at least 1"},
		"zero token ttl":                    {`token_ttl = "12h"`, `token_ttl = "0s"`, "auth.token_ttl must be positive"},
		"zero login code ttl":               {`login_code_ttl = "2m"`, `login_code_ttl = "0s"`, "auth.login_code_ttl must be positive"},
		"zero login code sweep":             {`login_code_sweep = "10m"`, `login_code_sweep = "0s"`, "auth.login_code_sweep must be positive"},
		"zero heartbeat":                    {`heartbeat = "25s"`, `heartbeat = "0s"`, "live.heartbeat must be positive"},
		"zero subscriber buffer":            {"subscriber_buffer = 16", "subscriber_buffer = 0", "live.subscriber_buffer must be at least 1"},
		"zero header timeout":               {`read_header_timeout = "10s"`, `read_header_timeout = "0s"`, "server.read_header_timeout must be positive"},
		"zero idle timeout":                 {`idle_timeout = "2m"`, `idle_timeout = "0s"`, "server.idle_timeout must be positive"},
		"zero shutdown timeout":             {`shutdown_timeout = "10s"`, `shutdown_timeout = "0s"`, "server.shutdown_timeout must be positive"},
		"zero write timeout":                {`write_timeout = "5s"`, `write_timeout = "0s"`, "storage.write_timeout must be positive"},
		"zero instance lock check":          {`instance_lock_check = "5s"`, `instance_lock_check = "0s"`, "storage.instance_lock_check must be positive"},
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
