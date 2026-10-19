package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// MaxTeamSize is the largest teams.size config.toml may set. It bounds the
// fixed-size arrays that hold a team's scan stack.
const MaxTeamSize = 16

// maxNameLength is the database's limit on team names (migration 0002).
const maxNameLength = 32

// Settings are the tunable rules and timings from config.toml. See that file
// for what each one means.
type Settings struct {
	// Teams are the team rules.
	Teams TeamSettings `toml:"teams"`
	// Game are the scanning rules.
	Game GameSettings `toml:"game"`
	// Auth are the sign-in and token timings.
	Auth AuthSettings `toml:"auth"`
	// Live are the event stream settings.
	Live LiveSettings `toml:"live"`
	// Server are the HTTP server's limits and timeouts.
	Server ServerSettings `toml:"server"`
	// Storage are the write-through settings.
	Storage StorageSettings `toml:"storage"`
}

// TeamSettings is config.toml's [teams] table.
type TeamSettings struct {
	// Size is players per team.
	Size int `toml:"size"`
	// NameMaxLength is the longest team name, in characters.
	NameMaxLength int `toml:"name_max_length"`
	// CodeAttempts is how many random team codes to try per creation.
	CodeAttempts int `toml:"code_attempts"`
}

// GameSettings is config.toml's [game] table.
type GameSettings struct {
	// ScanWindow is the span a pattern's scans must fit in, bottom to top.
	ScanWindow Duration `toml:"scan_window"`
	// ScanGrace is extra tolerance on ScanWindow.
	ScanGrace Duration `toml:"scan_grace"`
	// MaxPatternPoints is the most waypoints in a pattern, and the capacity of
	// each team's scan stack.
	MaxPatternPoints int `toml:"max_pattern_points"`
}

// AuthSettings is config.toml's [auth] table.
type AuthSettings struct {
	// TokenTTL is how long an access token is valid.
	TokenTTL Duration `toml:"token_ttl"`
	// LoginCodeTTL is how long a one-time login code is valid.
	LoginCodeTTL Duration `toml:"login_code_ttl"`
	// OAuthStateTTL is how long a sign-in may take on Google's screen.
	OAuthStateTTL Duration `toml:"oauth_state_ttl"`
	// LoginCodeSweep is how often expired login codes are deleted.
	LoginCodeSweep Duration `toml:"login_code_sweep"`
}

// LiveSettings is config.toml's [live] table.
type LiveSettings struct {
	// Heartbeat is how often idle streams get a comment.
	Heartbeat Duration `toml:"heartbeat"`
	// SubscriberBuffer is how many events a client may fall behind.
	SubscriberBuffer int `toml:"subscriber_buffer"`
	// HubShards is how many locks the hub spreads streams over.
	HubShards int `toml:"hub_shards"`
	// WriteTimeout is how long one write to a stream may take.
	WriteTimeout Duration `toml:"write_timeout"`
	// RetryMin is the shortest reconnect delay sent to a client.
	RetryMin Duration `toml:"retry_min"`
	// RetryMax is the longest reconnect delay sent to a client.
	RetryMax Duration `toml:"retry_max"`
}

// ServerSettings is config.toml's [server] table.
type ServerSettings struct {
	// ReadHeaderTimeout is how long a client may take to send headers.
	ReadHeaderTimeout Duration `toml:"read_header_timeout"`
	// ReadTimeout is how long a client may take to send a whole request.
	ReadTimeout Duration `toml:"read_timeout"`
	// IdleTimeout is how long an idle keep-alive connection stays open.
	IdleTimeout Duration `toml:"idle_timeout"`
	// ShutdownTimeout is how long shutdown waits for in-flight requests.
	ShutdownTimeout Duration `toml:"shutdown_timeout"`
	// MaxBodyBytes is the largest request body accepted.
	MaxBodyBytes int64 `toml:"max_body_bytes"`
}

// StorageSettings is config.toml's [storage] table.
type StorageSettings struct {
	// WriteTimeout bounds each write-through.
	WriteTimeout Duration `toml:"write_timeout"`
	// InstanceLockCheck is how often the single-instance lock is verified.
	InstanceLockCheck Duration `toml:"instance_lock_check"`
}

// Duration is a time.Duration written in config.toml as a string like "30s".
type Duration struct{ time.Duration }

// UnmarshalText parses a Go duration string.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// LoadSettings reads and validates the config.toml at path. Every key must be
// present and known; it returns every problem at once.
func LoadSettings(path string) (Settings, error) {
	var s Settings
	md, err := toml.DecodeFile(path, &s)
	if err != nil {
		return s, fmt.Errorf("read %s: %w", path, err)
	}
	var errs []error
	for _, key := range md.Undecoded() {
		errs = append(errs, fmt.Errorf("unknown key %s", key))
	}
	for _, key := range settingKeys() {
		if !md.IsDefined(key...) {
			errs = append(errs, fmt.Errorf("missing key %s", strings.Join(key, ".")))
		}
	}
	errs = append(errs, s.validate()...)
	if len(errs) > 0 {
		return s, fmt.Errorf("%s: %w", path, errors.Join(errs...))
	}
	return s, nil
}

// settingKeys lists every key Settings expects, as [table, key] from the
// toml tags, so a missing key is an error even when zero would be valid
// (e.g. game.scan_grace).
func settingKeys() [][]string {
	var keys [][]string
	top := reflect.TypeOf(Settings{})
	for i := range top.NumField() {
		table := top.Field(i)
		for j := range table.Type.NumField() {
			keys = append(keys, []string{table.Tag.Get("toml"), table.Type.Field(j).Tag.Get("toml")})
		}
	}
	return keys
}

// validate returns every out-of-range value, including missing ones (which
// decode as zero).
func (s Settings) validate() []error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	check(s.Teams.Size >= 1 && s.Teams.Size <= MaxTeamSize, "teams.size must be 1-%d", MaxTeamSize)
	check(s.Teams.NameMaxLength >= 1 && s.Teams.NameMaxLength <= maxNameLength,
		"teams.name_max_length must be 1-%d (the database's limit)", maxNameLength)
	check(s.Teams.CodeAttempts >= 1, "teams.code_attempts must be at least 1")
	check(s.Game.ScanWindow.Duration > 0, "game.scan_window must be positive")
	check(s.Game.ScanGrace.Duration >= 0, "game.scan_grace must not be negative")
	check(s.Game.MaxPatternPoints >= 1 && s.Game.MaxPatternPoints <= s.Teams.Size,
		"game.max_pattern_points must be 1-teams.size: each point needs a different member")
	check(s.Auth.TokenTTL.Duration > 0, "auth.token_ttl must be positive")
	check(s.Auth.LoginCodeTTL.Duration > 0, "auth.login_code_ttl must be positive")
	check(s.Auth.OAuthStateTTL.Duration >= time.Second, "auth.oauth_state_ttl must be at least 1s")
	check(s.Auth.LoginCodeSweep.Duration > 0, "auth.login_code_sweep must be positive")
	check(s.Server.ReadHeaderTimeout.Duration > 0, "server.read_header_timeout must be positive")
	check(s.Server.ReadTimeout.Duration >= s.Server.ReadHeaderTimeout.Duration, "server.read_timeout must be at least server.read_header_timeout")
	check(s.Storage.InstanceLockCheck.Duration > 0, "storage.instance_lock_check must be positive")
	check(s.Server.IdleTimeout.Duration > 0, "server.idle_timeout must be positive")
	check(s.Server.ShutdownTimeout.Duration > 0, "server.shutdown_timeout must be positive")
	check(s.Server.MaxBodyBytes >= 1, "server.max_body_bytes must be at least 1")
	check(s.Live.Heartbeat.Duration > 0, "live.heartbeat must be positive")
	check(s.Live.SubscriberBuffer >= 1, "live.subscriber_buffer must be at least 1")
	check(s.Live.HubShards >= 1, "live.hub_shards must be at least 1")
	check(s.Live.WriteTimeout.Duration > 0, "live.write_timeout must be positive")
	check(s.Live.RetryMin.Duration >= time.Millisecond, "live.retry_min must be at least 1ms")
	check(s.Live.RetryMax.Duration >= s.Live.RetryMin.Duration, "live.retry_max must be at least live.retry_min")
	check(s.Storage.WriteTimeout.Duration > 0, "storage.write_timeout must be positive")
	return errs
}

// SettingsPath is the config.toml to load: $CONFIG_FILE, or ./config.toml.
func SettingsPath() string {
	if p := os.Getenv("CONFIG_FILE"); p != "" {
		return p
	}
	return "config.toml"
}
