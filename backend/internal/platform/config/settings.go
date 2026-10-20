package config

import (
	"encoding"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// MaxTeamSize is the largest teams.size config.toml may set.
const MaxTeamSize = 16

// MaxPatternPoints is the largest game.max_pattern_points config.toml may
// set. It bounds the fixed-size array that holds a team's scan stack (a
// pattern's points plus one wrong scan), so it's a Go type size, not a
// tunable.
const MaxPatternPoints = 15

// maxNameLength is the database's limit on team names (migration 0002).
const maxNameLength = 32

// Settings are the tunable rules and timings from config.toml. See that file
// for what each one means.
type Settings struct {
	// Teams are the team rules.
	Teams TeamSettings `toml:"teams"`
	// Game are the scanning rules.
	Game GameSettings `toml:"game"`
	// QR are the rotating-QR rules.
	QR QRSettings `toml:"qr"`
	// Spell are the coefficients for pattern damage calculation.
	Spell SpellSettings `toml:"spell"`
	// Auth are the sign-in and token timings.
	Auth AuthSettings `toml:"auth"`
	// Live are the event stream settings.
	Live LiveSettings `toml:"live"`
	// Server are the HTTP server's limits and timeouts.
	Server ServerSettings `toml:"server"`
	// Storage are the write-through settings.
	Storage StorageSettings `toml:"storage"`
}

// SpellSettings is config.toml's [spell] table.
type SpellSettings struct {
	// Walkability scales straight-line distance to estimated walking distance.
	Walkability float64 `toml:"walkability"`
	// Per100m is damage per 100 m of estimated walking distance.
	Per100m float64 `toml:"per_100m"`
	// Base is base coordination damage by pattern point count ([spell.base]).
	Base map[string]int `toml:"base"`
	// Bonus holds bonus damage for difficult geometries ([spell.bonus]).
	Bonus SpellBonusSettings `toml:"bonus"`
}

// SpellBonusSettings is config.toml's [spell.bonus] table.
type SpellBonusSettings struct {
	// ConcaveQuad is bonus damage awarded to 4-point patterns whose convex hull has size 3.
	ConcaveQuad int `toml:"concave_quad"`
}

// BaseDamage returns Base keyed by point count as an int. validate has checked
// every key is a size from 1 to game.max_pattern_points.
func (s SpellSettings) BaseDamage() map[int]int { return intKeys(s.Base) }

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
	// MaxPatternPoints is the most waypoints in a pattern.
	MaxPatternPoints int `toml:"max_pattern_points"`
	// Bosses is how many bosses every team fights: the boss table must hold
	// exactly serials 1 to Bosses.
	Bosses int `toml:"bosses"`
	// Health is every team's starting health, and what a revive restores.
	Health int `toml:"health"`
	// LockedPatternPoints is the size from which patterns are locked until
	// the team has defeated UnlockAfterBosses bosses.
	LockedPatternPoints int `toml:"locked_pattern_points"`
	// UnlockAfterBosses is how many bosses a team must defeat to unlock
	// patterns of LockedPatternPoints or more waypoints.
	UnlockAfterBosses int `toml:"unlock_after_bosses"`
	// BossAttack is how bosses attack ([game.boss_attack]).
	BossAttack BossAttackSettings `toml:"boss_attack"`
}

// BossAttackSettings is [game.boss_attack]: every boss attacks a team every
// Interval of the team's fight against it, for its Damage.
type BossAttackSettings struct {
	// Interval is the time between a boss's attacks, the same for every
	// boss.
	Interval Duration `toml:"interval"`
	// Damage is each boss's attack damage by serial ([game.boss_attack.damage]:
	// "1" = 1, …): exactly the serials 1 to game.bosses.
	Damage map[string]int `toml:"damage"`
}

// BossDamage returns Damage keyed by serial as an int. validate has checked
// every key is a serial from 1 to game.bosses.
func (b BossAttackSettings) BossDamage() map[int]int { return intKeys(b.Damage) }

// intKeys returns m with its keys as ints. TOML table keys are strings, so
// tables keyed by a number ([game.boss_attack.damage], [spell.base]) decode as
// map[string]int; validate has checked every key with intKey, so none is
// skipped here in practice.
func intKeys(m map[string]int) map[int]int {
	out := make(map[int]int, len(m))
	for k, v := range m {
		if n, ok := intKey(k); ok {
			out[n] = v
		}
	}
	return out
}

// intKey parses k as a plain decimal int, as it would be written: "6", not
// "+6", "06" or "6x". ok is false otherwise.
func intKey(k string) (int, bool) {
	n, err := strconv.Atoi(k)
	return n, err == nil && strconv.Itoa(n) == k
}

// QRSettings is config.toml's [qr] table.
type QRSettings struct {
	// Rotate is how often a waypoint's rotating QR changes.
	Rotate Duration `toml:"rotate"`
	// Tolerance is how much longer than rotate a scan stays accepted.
	Tolerance Duration `toml:"tolerance"`
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

// settingKeys lists every key Settings expects, as its path of toml tags
// ([table, key], or deeper for a nested table such as
// [game, boss_attack, interval]), so a missing key is an error even when
// zero would be valid. A struct field is a nested table, unless it decodes
// from text itself (Duration); a map is one key holding a table of its own.
func settingKeys() [][]string {
	var keys [][]string
	var walk func(t reflect.Type, path []string)
	walk = func(t reflect.Type, path []string) {
		for i := range t.NumField() {
			f := t.Field(i)
			key := append(append([]string{}, path...), f.Tag.Get("toml"))
			if f.Type.Kind() == reflect.Struct && !reflect.PointerTo(f.Type).Implements(textUnmarshaler) {
				walk(f.Type, key)
				continue
			}
			keys = append(keys, key)
		}
	}
	walk(reflect.TypeOf(Settings{}), nil)
	return keys
}

// textUnmarshaler is encoding.TextUnmarshaler's type: a struct that
// implements it (Duration) is a value, not a nested table.
var textUnmarshaler = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()

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
	check(s.Game.MaxPatternPoints >= 1 && s.Game.MaxPatternPoints <= MaxPatternPoints,
		"game.max_pattern_points must be 1-%d", MaxPatternPoints)
	check(s.Game.Bosses >= 1, "game.bosses must be at least 1")
	check(s.Game.Health >= 1, "game.health must be at least 1")
	check(s.Game.LockedPatternPoints >= 1 && s.Game.LockedPatternPoints <= s.Game.MaxPatternPoints+1,
		"game.locked_pattern_points must be 1-%d (game.max_pattern_points + 1 locks nothing)", s.Game.MaxPatternPoints+1)
	check(s.Game.UnlockAfterBosses >= 0 && s.Game.UnlockAfterBosses <= s.Game.Bosses,
		"game.unlock_after_bosses must be 0-%d (game.bosses)", s.Game.Bosses)
	check(s.Game.BossAttack.Interval.Duration > 0, "game.boss_attack.interval must be positive")
	for serial := 1; serial <= s.Game.Bosses; serial++ {
		check(hasKey(s.Game.BossAttack.Damage, fmt.Sprint(serial)), "game.boss_attack.damage has no entry for boss %d", serial)
	}
	for k, v := range s.Game.BossAttack.Damage {
		serial, ok := intKey(k)
		check(ok && serial >= 1 && serial <= s.Game.Bosses,
			"game.boss_attack.damage.%s is not a boss serial (1-%d, game.bosses)", k, s.Game.Bosses)
		check(v >= 0, "game.boss_attack.damage.%s must not be negative", k)
	}
	check(s.QR.Rotate.Duration > 0, "qr.rotate must be positive")
	check(s.QR.Tolerance.Duration > 0, "qr.tolerance must be positive")
	check(s.Spell.Walkability >= 1.0, "spell.walkability must be at least 1.0")
	check(s.Spell.Per100m >= 0, "spell.per_100m must not be negative")
	for pts := 1; pts <= s.Game.MaxPatternPoints; pts++ {
		check(hasKey(s.Spell.Base, fmt.Sprint(pts)), "spell.base has no entry for size %d", pts)
	}
	for k, v := range s.Spell.Base {
		pts, ok := intKey(k)
		check(ok && pts >= 1 && pts <= s.Game.MaxPatternPoints,
			"spell.base.%s is not a valid pattern size (1-%d, game.max_pattern_points)", k, s.Game.MaxPatternPoints)
		check(v > 0, "spell.base.%s must be positive", k)
	}
	check(s.Spell.Bonus.ConcaveQuad >= 0, "spell.bonus.concave_quad must not be negative")
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

// hasKey reports whether m has key k.
func hasKey(m map[string]int, k string) bool {
	_, ok := m[k]
	return ok
}
