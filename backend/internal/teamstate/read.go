package teamstate

import (
	"math"
	"slices"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/atlas"
	"labyrinth/internal/platform/config"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/teams"
)

// State is everything a team has done in the game. Snapshots are never
// modified once stored; writers build a new one.
type State struct {
	// Waypoints is every waypoint the team has ever had a scan accepted at.
	Waypoints atlas.WaypointSet
	// Patterns is every pattern the team has completed.
	Patterns atlas.PatternSet
	// Shapes is every shape the team has completed at least one pattern of.
	Shapes atlas.ShapeSet
	// Selection is the shape the team is attempting, if any.
	Selection Selection
	// Stack is the team's accepted scans since it selected its shape: what
	// completion is checked against.
	Stack Stack
	// Casts is every pattern the team has completed, repeats included,
	// oldest first: the cast log (team_cast). A write never appends to the
	// current snapshot's slice; it builds a new one (copy-on-write), so
	// snapshots stay immutable.
	Casts []Cast
	// Boss is the team's progress through the bosses (bosses use case).
	Boss BossProgress
	// QRCooldowns are the waypoints the team can't scan yet, soonest to open
	// first: those of its casts less than game.qr_cooldown ago. Derived from
	// Casts (qrCooldowns), so nothing more is stored; expired entries are
	// dropped by catchUp, which publishes qr_cooldown_ended. A write builds
	// a new slice (copy-on-write).
	QRCooldowns []QRCooldown
	// CooldownsReset is how many casts the log held when an admin last
	// reset the team's cooldowns (ResetCooldowns): only the casts after
	// those (Casts[CooldownsReset:]) put anything on cooldown, class or QR.
	// 0 if never. A position, not a time, so a cast at the same instant as
	// the reset still counts. Stored in team_boss.cooldowns_reset_casts.
	CooldownsReset int
	// Powerups are the powerups the team holds and the ones running.
	Powerups Powerups
	// Version orders this team's snapshots (see live.Version): every apply
	// sets the next one. Memory-only; it starts again, in a new Epoch, after a
	// restart.
	Version live.Version
}

// Poisoned reports whether the stack holds a wrong scan: the attempt can't
// cast, and the team must select again.
func (st State) Poisoned() bool {
	for i := range st.Stack.Len() {
		if st.Stack.At(i).Wrong {
			return true
		}
	}
	return false
}

// LastCast returns the team's most recent cast; ok is false if it has none.
func (st State) LastCast() (c Cast, ok bool) {
	if len(st.Casts) == 0 {
		return Cast{}, false
	}
	return st.Casts[len(st.Casts)-1], true
}

// Powerup is a kind of powerup: a temporary effect a team holds (at most one
// of each kind) and its leader activates. Each kind is a rule of its own, so
// the kinds are fixed in code; their strengths and durations are in
// config.toml ([powerups]).
type Powerup uint8

const (
	// PowerupDamageMultiplier: the team's next cast after activation deals
	// × Rules.PowerupMultiplier, then it's used up.
	PowerupDamageMultiplier Powerup = iota
	// PowerupHeart gives back Rules.PowerupHeart health, never above
	// Rules.Health.
	PowerupHeart
	// PowerupShortQRCooldown: for Rules.ShortQRWindow after activation, the
	// team's casts cool their waypoints for Rules.ShortQRCooldown instead of
	// Rules.QRCooldown.
	PowerupShortQRCooldown
	// PowerupInstantCast casts a pattern the leader names at once, with no
	// scans, whatever the cooldowns.
	PowerupInstantCast
	// powerupKinds is how many kinds there are: the length of powerupIDs and
	// PowerupSet's range. A code fact (each kind is a rule), not config.
	powerupKinds
)

// powerupIDs are the kinds' wire names, by kind: team_powerup.kind and what
// the API takes and shows.
var powerupIDs = [powerupKinds]string{"damage_multiplier", "heart", "short_qr_cooldown", "instant_cast"}

// PowerupIDs returns every powerup kind's wire name, in kind order: what
// server.Build gives the qte use case, so a QTE can only earn a real kind.
func PowerupIDs() []string { return powerupIDs[:] }

// ID returns k's wire name, e.g. "heart".
func (k Powerup) ID() string { return powerupIDs[k] }

// PowerupByID returns the kind whose wire name is id; ok is false for an
// unknown name.
func PowerupByID(id string) (k Powerup, ok bool) {
	for i, name := range powerupIDs {
		if name == id {
			return Powerup(i), true
		}
	}
	return 0, false
}

// PowerupSet is a set of powerup kinds: bit k for kind k.
type PowerupSet uint8

// Has reports whether k is in s.
func (s PowerupSet) Has(k Powerup) bool { return s&(1<<k) != 0 }

// With returns s with k added.
func (s PowerupSet) With(k Powerup) PowerupSet { return s | 1<<k }

// Without returns s with k removed.
func (s PowerupSet) Without(k Powerup) PowerupSet { return s &^ (1 << k) }

// Powerups are a team's powerups: the kinds it holds and the effects
// running. A value inside State, copied with it.
type Powerups struct {
	// Held are the kinds the team holds, at most one of each
	// (team_powerup.held).
	Held PowerupSet
	// MultiplierArmed reports that a damage_multiplier powerup is active:
	// the team's next cast deals × Rules.PowerupMultiplier and uses it up.
	// Derived: team_powerup.activated_casts equals the cast log's length.
	MultiplierArmed bool
	// ShortQRUntil is when the running short_qr_cooldown powerup stops:
	// casts before it cool their waypoints for Rules.ShortQRCooldown. Zero
	// when none is running; catchUp clears it once passed. Derived:
	// team_powerup.activated_at + Rules.ShortQRWindow.
	ShortQRUntil time.Time
}

// QRCooldown is a waypoint a team can't scan until Until: it was in one of
// the team's casts less than game.qr_cooldown ago (or
// powerups.short_qr_cooldown, for a cast during a short_qr_cooldown powerup).
type QRCooldown struct {
	// Waypoint is the waypoint's index in the atlas.
	Waypoint int
	// Until is when it opens again: the latest end among its casts'
	// cooldowns. It's on cooldown while now is before Until.
	Until time.Time
}

// qrCooldownUntil returns when waypoint w opens again for st's team, and
// whether it's on cooldown at now.
func (st State) qrCooldownUntil(w int, now time.Time) (time.Time, bool) {
	for _, c := range st.QRCooldowns {
		if c.Waypoint == w && now.Before(c.Until) {
			return c.Until, true
		}
	}
	return time.Time{}, false
}

// Cast is one completed pattern: the moment a team's stack became exactly a
// pattern of its selected shape.
type Cast struct {
	// Pattern is the pattern's index in the atlas.
	Pattern int
	// By is the member whose scan completed it.
	By uuid.UUID
	// At is when.
	At time.Time
	// AlreadyCompleted reports that the team had cast this pattern before,
	// so its progress didn't grow.
	AlreadyCompleted bool
	// Dealt is the damage the cast dealt to a boss: the pattern's damage ×
	// the team's multiplier at the time, rounded; 0 if every boss was
	// already defeated.
	Dealt int
	// Boss is the serial of the boss it hit, or 0 if none.
	Boss int
	// DefeatedBoss reports that this cast finished Boss.
	DefeatedBoss bool
	// Instant reports that an instant_cast powerup cast it, with no scans.
	Instant bool
	// Boosted reports that a damage_multiplier powerup multiplied Dealt.
	Boosted bool
	// ShortQR reports that a short_qr_cooldown powerup was active: its
	// waypoints cool for Rules.ShortQRCooldown, not Rules.QRCooldown.
	ShortQR bool
}

// BossProgress is a team's progress through the bosses, which every team
// fights in serial order, and the team's health, which they attack. A value,
// so a State copy copies it.
//
// Health is derived, never stored as it changes. Each boss's phase begins at
// PhaseStart with PhaseHealth (boss 1: the team's start with full health;
// boss n+1: the cast that defeated boss n; or a revive), and the boss
// attacks at PhaseStart + k × its interval, k = 1, 2, …. So at any time
// (at):
//
//	Attacks = min(floor((at − PhaseStart) / interval), the knockout attack)
//	Health  = max(0, PhaseHealth − Attacks × attack damage)
//
// where the knockout attack is ceil(PhaseHealth / attack damage): the
// attack that brings health to 0, after which the boss stops.
//
// Only PhaseStart and PhaseHealth are persisted (team_boss); Attacks and
// Health are recomputed by at, so a timer's attack is a memory-only change
// that Load reproduces exactly.
type BossProgress struct {
	// Defeated is how many bosses the team has defeated; it's fighting the
	// boss at index Defeated (serial Defeated+1), or none once Defeated is
	// the boss count.
	Defeated int
	// Damage is what the team has dealt to its current boss so far. A cast
	// that brings it to the boss's HP defeats the boss and resets it to 0:
	// damage beyond the HP is lost, not carried to the next boss.
	Damage int
	// Multiplier is what each cast's damage is multiplied by before it's
	// dealt (rounded to an integer). 1 unless something sets it; nothing
	// does yet.
	Multiplier float64
	// PhaseStart is when the current boss's phase began.
	PhaseStart time.Time
	// PhaseHealth is the team's health when the phase began.
	PhaseHealth int
	// Attacks is how many times the current boss has attacked this phase,
	// as of the last at; derived.
	Attacks int
	// Health is the team's health as of the last at; derived. At 0 the
	// team is knocked out.
	Health int
}

// KnockedOut reports whether the team's health is 0: it can't select or
// scan, and nothing attacks it, until an admin revives it.
func (p BossProgress) KnockedOut() bool { return p.Health <= 0 }

// at returns p with Attacks and Health as of now, against bosses. The boss
// stops attacking at the knockout (the attack that brings health to 0), so
// Attacks is capped there, and once every boss is defeated nothing changes.
// A pure function of the stored phase and now, so calling it again (a
// timer, a write, Load after a restart) gives the same answer however it's
// reached.
func (p BossProgress) at(now time.Time, b Bosses) BossProgress {
	if p.Defeated >= b.Count() {
		return p
	}
	boss := b.At(p.Defeated)
	attacks := 0
	if elapsed := now.Sub(p.PhaseStart); elapsed > 0 {
		attacks = int(elapsed / boss.AttackInterval())
	}
	if boss.AttackDamage > 0 {
		attacks = min(attacks, (p.PhaseHealth+boss.AttackDamage-1)/boss.AttackDamage)
	}
	p.Attacks = attacks
	p.Health = max(0, p.PhaseHealth-attacks*boss.AttackDamage)
	return p
}

// nextAttack returns when the current boss attacks next: PhaseStart +
// (Attacks+1) × its interval. ok is false if nothing will attack (every
// boss defeated, or the team knocked out).
func (p BossProgress) nextAttack(b Bosses) (at time.Time, ok bool) {
	if p.Defeated >= b.Count() || p.KnockedOut() {
		return time.Time{}, false
	}
	return p.PhaseStart.Add(time.Duration(p.Attacks+1) * b.At(p.Defeated).AttackInterval()), true
}

// deal applies a cast of the given damage at now to p, against bosses: the
// new progress, the damage dealt (damage × multiplier × factor, rounded,
// multiplied in that order; factor is a damage_multiplier powerup's, or 1),
// the serial
// of the boss hit (0 if every boss is already defeated, in which case
// nothing is dealt), and whether the cast defeated it. A defeat starts the
// next boss's phase at now, with the team's current health. p must already
// be at now.
func deal(p BossProgress, damage int, factor float64, now time.Time, b Bosses) (next BossProgress, dealt, serial int, defeated bool) {
	if p.Defeated >= b.Count() {
		return p, 0, 0, false
	}
	boss := b.At(p.Defeated)
	dealt = int(math.Round(float64(damage) * p.Multiplier * factor))
	next = p
	next.Damage += dealt
	if next.Damage >= boss.HP {
		next.Defeated++
		next.Damage = 0 // the rest of this cast is lost
		next.PhaseStart, next.PhaseHealth, next.Attacks = now, p.Health, 0
		defeated = true
	}
	return next, dealt, boss.Serial, defeated
}

// Selection is the shape a team has chosen to attempt.
type Selection struct {
	// Active reports whether a shape is selected; the other fields are zero
	// when it isn't.
	Active bool
	// Shape is the selected shape's index in the atlas.
	Shape int
	// By is the member who selected it.
	By uuid.UUID
	// At is when it was selected.
	At time.Time
}

// StackScan is one accepted scan in a team's stack.
type StackScan struct {
	// Waypoint is the waypoint's index in the atlas.
	Waypoint int
	// UserID is the member who scanned it.
	UserID uuid.UUID
	// At is when it was scanned.
	At time.Time
	// Wrong means the scan didn't fit the selected shape's patterns (with the
	// scans before it). It poisons the attempt: nothing more can be scanned
	// until the team selects again. Shown to players (StackScanView.Wrong).
	Wrong bool
}

// Stack is a team's accepted scans since it selected its shape, oldest at the
// bottom and newest on top: one scan per waypoint (any member may scan any
// number of them), all within one pattern of the shape, plus at most one
// wrong scan on top. So it never holds more than Cap = game.max_pattern_points
// + 1. There's no time limit: scans stay until the attempt casts or the team
// selects again.
//
// It's a value: Push returns a new Stack and leaves the receiver unchanged,
// so snapshots stay immutable.
type Stack struct {
	// scans is the storage; only Cap slots are used. Sized for the largest
	// allowed pattern plus one wrong scan.
	scans [config.MaxPatternPoints + 1]StackScan
	// n is how many scans the stack holds, 0…Cap.
	n int
	// Cap is the stack's capacity.
	Cap int
}

// NewStack returns an empty stack with room for a pattern of maxPoints
// waypoints plus one wrong scan.
func NewStack(maxPoints int) Stack { return Stack{Cap: maxPoints + 1} }

// Len returns how many scans the stack holds.
func (s Stack) Len() int { return s.n }

// At returns the i-th scan from the bottom: At(0) is the oldest, At(Len()-1)
// the top.
func (s Stack) At(i int) StackScan { return s.scans[i] }

// Push returns the stack with sc on top. Scan never pushes onto a full stack
// (correct scans fit one pattern, and nothing is pushed after a wrong one);
// Push panics if it's asked to, since that would be a bug.
func (s Stack) Push(sc StackScan) Stack {
	if s.n == s.Cap {
		panic("teamstate: push onto a full stack")
	}
	s.scans[s.n] = sc
	s.n++
	return s
}

// View is a team's State as the API and live events show it: IDs and
// timestamps instead of indexes and bits.
type View struct {
	// TeamID is the team the state belongs to.
	TeamID string `json:"team_id"`
	// Waypoints is every waypoint ever accepted, sorted.
	Waypoints []string `json:"waypoints"`
	// Patterns is every completed pattern, each as its sorted waypoint IDs.
	Patterns [][]string `json:"patterns"`
	// Shapes is every completed shape, in index order.
	Shapes []string `json:"shapes"`
	// Selection is the selected shape, or null.
	Selection *SelectionView `json:"selection"`
	// LastCast is the team's most recent cast, or null if it has none. The
	// full history is GET /api/teams/me/casts.
	LastCast *CastView `json:"last_cast"`
	// Stack is the attempt's scans, bottom (oldest) to top (newest), each
	// marked wrong if it didn't fit.
	Stack []StackScanView `json:"stack"`
	// Poisoned reports that a scan in the stack is wrong: nothing more can be
	// scanned until the team selects again.
	Poisoned bool `json:"poisoned"`
	// BossesDefeated is how many bosses the team has defeated.
	BossesDefeated int `json:"bosses_defeated"`
	// Boss is the boss the team is fighting and the damage dealt to it so
	// far, or null once every boss is defeated. The boss itself (name, HP,
	// asset) is in GET /api/game/bosses.
	Boss *BossView `json:"boss"`
	// DamageMultiplier is what each cast's damage is multiplied by before
	// it's dealt to the boss (rounded): 1 unless something sets it.
	DamageMultiplier float64 `json:"damage_multiplier"`
	// Health is the team's health: game.health at the start, less every
	// boss attack. At 0 the team is knocked out.
	Health int `json:"health"`
	// KnockedOut reports health 0: selecting and scanning are refused
	// (knocked_out) and nothing attacks until an admin revives the team.
	KnockedOut bool `json:"knocked_out"`
	// UnlockedPoints is the most waypoints a pattern the team can cast may
	// have: game.locked_pattern_points − 1 until it has defeated
	// game.unlock_after_bosses bosses, then game.max_pattern_points.
	UnlockedPoints int `json:"unlocked_points"`
	// LockedShapes are the shapes the team can't select yet: every one of
	// their patterns is bigger than UnlockedPoints. In index order.
	LockedShapes []string `json:"locked_shapes"`
	// CooldownClass is the spell class the team can't cast now: the class
	// of its last cast, until it casts a spell of another class. Null
	// before the first cast.
	CooldownClass *string `json:"cooldown_class"`
	// CooldownShapes are the shapes the team can't select because of the
	// cooldown: every one of their patterns it has unlocked is of
	// CooldownClass. Not in LockedShapes. In index order.
	CooldownShapes []string `json:"cooldown_shapes"`
	// QRCooldowns are the waypoints the team can't scan yet (409
	// waypoint_on_cooldown), each with when it opens again: those of its
	// casts less than game.qr_cooldown ago. Soonest first, then by ID.
	QRCooldowns []QRCooldownView `json:"qr_cooldowns"`
	// CastCount is how many casts are in the team's cast log, repeats
	// included. A client holding the log (GET /api/teams/me/casts) re-fetches
	// it when this is more than its length plus the cast it just saw.
	CastCount int `json:"cast_count"`
	// Powerups are the powerups the team holds and the ones running.
	Powerups PowerupsView `json:"powerups"`
	// Version orders this team's snapshots: keep a copy only if its epoch
	// differs from the one held, or it has a higher seq (live.Version).
	Version live.Version `json:"version"`
}

// CastView is a Cast as JSON.
type CastView struct {
	// Pattern is the cast pattern's waypoint IDs, sorted.
	Pattern []string `json:"pattern"`
	// Shape is the pattern's shape ID.
	Shape string `json:"shape"`
	// Class is the pattern's spell class ID, e.g. "class_4": the class of
	// patterns with its waypoint count.
	Class string `json:"class"`
	// CastBy is the member whose scan completed it.
	CastBy uuid.UUID `json:"cast_by"`
	// CastAt is when.
	CastAt time.Time `json:"cast_at"`
	// AlreadyCompleted reports that the team had cast this pattern before.
	AlreadyCompleted bool `json:"already_completed"`
	// Damage is what the cast spell deals: the pattern's damage.
	Damage int `json:"damage"`
	// Dealt is the damage it dealt to a boss: Damage × the team's multiplier
	// then, rounded; 0 if every boss was already defeated.
	Dealt int `json:"dealt"`
	// Boss is the serial of the boss it hit, or null if none.
	Boss *int `json:"boss"`
	// DefeatedBoss reports that this cast defeated that boss.
	DefeatedBoss bool `json:"defeated_boss"`
	// Instant reports that an instant_cast powerup cast it, with no scans.
	Instant bool `json:"instant"`
	// Boosted reports that a damage_multiplier powerup multiplied Dealt.
	Boosted bool `json:"boosted"`
	// ShortQRCooldown reports that its waypoints cool for
	// powerups.short_qr_cooldown (a short_qr_cooldown powerup was active).
	ShortQRCooldown bool `json:"short_qr_cooldown"`
}

// PowerupsView is a team's Powerups as JSON.
type PowerupsView struct {
	// Held are the kinds the team holds, in kind order; [] when none.
	Held []string `json:"held"`
	// Active are the effects running, in kind order; [] when none. A heart
	// or an instant cast takes effect at once, so never shows here.
	Active []ActivePowerupView `json:"active"`
}

// ActivePowerupView is one running powerup effect.
type ActivePowerupView struct {
	// Kind is the powerup's wire name.
	Kind string `json:"kind"`
	// Until is when it stops, or null for one used up by the next cast
	// (damage_multiplier).
	Until *time.Time `json:"until"`
}

// BossView is the boss a team is fighting, as JSON.
type BossView struct {
	// Serial is the boss's serial (see GET /api/game/bosses).
	Serial int `json:"serial"`
	// Damage is what the team has dealt to it so far, below its HP.
	Damage int `json:"damage"`
	// PhaseStartedAt is when the team began fighting it (or was last
	// revived): its attack clock counts from here.
	PhaseStartedAt time.Time `json:"phase_started_at"`
	// Attacks is how many times it has attacked this phase.
	Attacks int `json:"attacks"`
	// NextAttackAt is when it attacks next, or null if the team is knocked
	// out. Clients count down to it on the server's clock (hello).
	NextAttackAt *time.Time `json:"next_attack_at"`
}

// SelectionView is a Selection as JSON.
type SelectionView struct {
	// Shape is the selected shape's ID.
	Shape string `json:"shape"`
	// SelectedBy is the member who selected it.
	SelectedBy uuid.UUID `json:"selected_by"`
	// SelectedAt is when it was selected.
	SelectedAt time.Time `json:"selected_at"`
}

// QRCooldownView is a QRCooldown as JSON.
type QRCooldownView struct {
	// WaypointID is the waypoint that can't be scanned yet.
	WaypointID string `json:"waypoint_id"`
	// Until is when the team can scan it again.
	Until time.Time `json:"until"`
}

// StackScanView is a StackScan as JSON.
type StackScanView struct {
	// WaypointID is the scanned waypoint's 4-digit ID.
	WaypointID string `json:"waypoint_id"`
	// UserID is the member who scanned it.
	UserID uuid.UUID `json:"user_id"`
	// ScannedAt is when it was scanned.
	ScannedAt time.Time `json:"scanned_at"`
	// Wrong reports that the scan didn't fit the selected shape (with the
	// scans before it); it poisons the attempt.
	Wrong bool `json:"wrong"`
}

// DamageRange is the minimum and maximum damage across a shape's patterns.
type DamageRange struct {
	// Min is the minimum damage across the shape's patterns.
	Min int `json:"min"`
	// Max is the maximum damage across the shape's patterns.
	Max int `json:"max"`
}

// Game is what a client needs before playing, from GET /api/game/shapes: the
// shapes a team can select. It's the same for everyone and never changes
// while the backend runs.
type Game struct {
	// Shapes are the selectable shapes, in index order.
	Shapes []GameShape `json:"shapes"`
	// Health is game.health: every team's starting health, and what a
	// revive restores.
	Health int `json:"health"`
	// LockedPatternPoints is game.locked_pattern_points: patterns of at
	// least this many waypoints are locked until the team has defeated
	// UnlockAfterBosses bosses.
	LockedPatternPoints int `json:"locked_pattern_points"`
	// UnlockAfterBosses is game.unlock_after_bosses.
	UnlockAfterBosses int `json:"unlock_after_bosses"`
	// MaxPatternPoints is game.max_pattern_points.
	MaxPatternPoints int `json:"max_pattern_points"`
	// SpellClasses are every spell class, ascending by points: a pattern of
	// n waypoints is in the class whose points is n.
	SpellClasses []atlas.SpellClass `json:"spell_classes"`
	// Powerups are what each powerup does ([powerups]).
	Powerups GamePowerups `json:"powerups"`
}

// GamePowerups are the powerups' strengths and durations, from config.toml.
type GamePowerups struct {
	// DamageMultiplier is what a damage_multiplier powerup multiplies the
	// next cast's damage by.
	DamageMultiplier float64 `json:"damage_multiplier"`
	// Heart is the health a heart powerup gives back.
	Heart int `json:"heart"`
	// ShortQRCooldownS is how long, in seconds, a cast's waypoints cool
	// while a short_qr_cooldown powerup is active.
	ShortQRCooldownS int `json:"short_qr_cooldown_s"`
	// ShortQRWindowS is how long, in seconds, a short_qr_cooldown powerup
	// stays active.
	ShortQRWindowS int `json:"short_qr_window_s"`
}

// GameShape is one selectable shape: what the client calls a pattern type.
type GameShape struct {
	// ID is what PUT /api/teams/me/state/selection takes, e.g.
	// "quadrilateral".
	ID string `json:"id"`
	// Name is for display, e.g. "Quadrilateral".
	Name string `json:"name"`
	// Patterns is how many valid patterns the shape has.
	Patterns int `json:"patterns"`
	// Points is the distinct sizes of its patterns, ascending, e.g. [4] (one
	// per shape in the 2026 seed).
	Points []int `json:"points"`
	// Damage is the min and max damage over the shape's patterns.
	Damage DamageRange `json:"damage"`
	// PatternsByPoints counts its patterns by size: {"3": 12, "4": 6}. A
	// team can cast those of at most its team_state.unlocked_points.
	PatternsByPoints map[int]int `json:"patterns_by_points"`
	// Classes are the spell classes of its patterns, ascending by points,
	// one per entry of Points, e.g. ["class_3", "class_4"].
	Classes []string `json:"classes"`
}

// unlockedPoints returns the most waypoints a pattern st's team can cast may
// have: everything (game.max_pattern_points) once it has defeated
// game.unlock_after_bosses bosses, game.locked_pattern_points − 1 before.
func (s *Store) unlockedPoints(st State) int {
	if st.Boss.Defeated >= s.rules.UnlockAfterBosses {
		return s.rules.MaxPatternPoints
	}
	return s.rules.LockedPatternPoints - 1
}

// shapeLocked reports whether every pattern of shape is bigger than
// unlocked, so a team with that limit can't select it. A shape with no
// patterns isn't locked (there's nothing to unlock).
func (s *Store) shapeLocked(shape, unlocked int) bool {
	points := s.game.Shapes[shape].Points // ascending
	return len(points) > 0 && points[0] > unlocked
}

// qrCooldowns derives the QR cooldowns at now from a team's cast log: every
// waypoint of a cast less than game.qr_cooldown before now, until that cast's
// time plus game.qr_cooldown (the latest cast decides), soonest first, ties
// by waypoint index. Nil when game.qr_cooldown is 0. The log is oldest first,
// so it walks back from the newest cast and stops at the first one too old.
//
// Only casts from index reset on count (an admin's ResetCooldowns).
func (s *Store) qrCooldowns(casts []Cast, reset int, now time.Time) []QRCooldown {
	longest := max(s.rules.QRCooldown, s.rules.ShortQRCooldown)
	if longest <= 0 {
		return nil
	}
	var until [atlas.MaxWaypoints]time.Time // by waypoint index: the latest end so far
	var cooling atlas.WaypointSet
	for i := len(casts) - 1; i >= reset; i-- {
		c := casts[i]
		if !now.Before(c.At.Add(longest)) {
			break // this cast and every older one are past any cooldown, or reset
		}
		end := c.At.Add(s.rules.QRCooldown)
		if c.ShortQR {
			end = c.At.Add(s.rules.ShortQRCooldown)
		}
		if !now.Before(end) {
			continue
		}
		for _, w := range s.atlas.Pattern(c.Pattern).Waypoints.Indexes() {
			if end.After(until[w]) {
				until[w] = end
			}
			cooling = cooling.With(w)
		}
	}
	var out []QRCooldown
	for _, w := range cooling.Indexes() {
		out = append(out, QRCooldown{Waypoint: w, Until: until[w]})
	}
	slices.SortFunc(out, func(a, b QRCooldown) int {
		if c := a.Until.Compare(b.Until); c != 0 {
			return c
		}
		return a.Waypoint - b.Waypoint
	})
	return out
}

// cooldown returns the spell class st's team can't cast now: its last
// cast's class, or "" before its first cast. Derived from the cast log, so a
// restart reproduces it with nothing more stored.
func (s *Store) cooldown(st State) string {
	lc, ok := st.LastCast()
	if !ok || len(st.Casts) <= st.CooldownsReset { // none, or none since the reset
		return ""
	}
	return s.atlas.Pattern(lc.Pattern).Spell.Class
}

// shapeOnCooldown reports whether shape has patterns of at most unlocked
// waypoints but every one of them is of spell class cooldown, so a team
// with that limit and cooldown can't select it. False for a locked shape
// (shapeLocked says so) and when cooldown is "".
func (s *Store) shapeOnCooldown(shape, unlocked int, cooldown string) bool {
	if cooldown == "" {
		return false
	}
	sh := s.game.Shapes[shape]
	within := false
	for i, n := range sh.Points { // a shape's sizes and their classes, ascending
		if n > unlocked {
			break
		}
		within = true
		if sh.Classes[i] != cooldown {
			return false
		}
	}
	return within
}

// Game returns the shapes for GET /api/game/shapes.
func (s *Store) Game() Game { return s.game }

// newGame builds the Game from the atlas and rules.
func newGame(a Atlas, r Rules) Game {
	g := Game{
		Shapes:              make([]GameShape, a.ShapeCount()),
		MaxPatternPoints:    r.MaxPatternPoints,
		Health:              r.Health,
		LockedPatternPoints: r.LockedPatternPoints,
		UnlockAfterBosses:   r.UnlockAfterBosses,
		SpellClasses:        a.SpellClasses(),
		Powerups: GamePowerups{DamageMultiplier: r.PowerupMultiplier, Heart: r.PowerupHeart,
			ShortQRCooldownS: int(r.ShortQRCooldown.Seconds()), ShortQRWindowS: int(r.ShortQRWindow.Seconds())},
	}
	for i := range a.ShapeCount() {
		sh := a.Shape(i)
		pIndices := a.ShapePatterns(i)
		sizes := map[int]bool{}
		minD, maxD := 0, 0
		if len(pIndices) > 0 {
			minD = a.Pattern(pIndices[0]).Spell.Damage
			maxD = minD
			for _, p := range pIndices[1:] {
				dmg := a.Pattern(p).Spell.Damage
				if dmg < minD {
					minD = dmg
				}
				if dmg > maxD {
					maxD = dmg
				}
			}
		}
		byPoints := map[int]int{}
		classBySize := map[int]string{}
		for _, p := range pIndices {
			pat := a.Pattern(p)
			sizes[pat.Waypoints.Len()] = true
			byPoints[pat.Waypoints.Len()]++
			classBySize[pat.Waypoints.Len()] = pat.Spell.Class
		}
		points := make([]int, 0, len(sizes))
		for n := range sizes {
			points = append(points, n)
		}
		slices.Sort(points)
		classes := make([]string, len(points))
		for j, n := range points {
			classes[j] = classBySize[n]
		}
		g.Shapes[i] = GameShape{
			ID:               sh.ID,
			Name:             sh.Name,
			Patterns:         len(pIndices),
			Points:           points,
			Damage:           DamageRange{Min: minD, Max: maxD},
			PatternsByPoints: byPoints,
			Classes:          classes,
		}
	}
	return g
}

// ErrNotInTeam is the same error the teams use case returns.
var ErrNotInTeam = teams.ErrNotInTeam

// All reads come from memory. The atlas is immutable and needs no lock; a
// team's state is a lock-free load of its snapshot.

// ForTeam returns a team's state; a team that hasn't done anything yet has
// an empty State.
func (s *Store) ForTeam(teamID string) State {
	s.mu.RLock()
	e := s.states[teamID]
	s.mu.RUnlock()
	if e == nil {
		return s.emptyState(teamID)
	}
	return *e.cur.Load()
}

// TeamView returns teamID's state as the API shows it (the admin's view of
// a team); a team that hasn't done anything has the empty state's view.
func (s *Store) TeamView(teamID string) View { return s.view(teamID, s.ForTeam(teamID)) }

// ForUser returns the state of userID's team, or ErrNotInTeam.
func (s *Store) ForUser(userID uuid.UUID) (View, error) {
	t, err := s.teams.ForUser(userID)
	if err != nil {
		return View{}, err
	}
	return s.view(t.ID, s.ForTeam(t.ID)), nil
}

// Snapshot is userID's team state as a live event, sent first on a new event
// stream. ok is false when they aren't in a team.
func (s *Store) Snapshot(userID uuid.UUID) (live.Event, bool) {
	t, err := s.teams.ForUser(userID)
	if err != nil {
		return live.Event{}, false
	}
	st := s.ForTeam(t.ID)
	ev, err := s.event(t.ID, &st, live.Snapshot)
	if err != nil {
		s.log.Error("encode team state event", "error", err)
		return live.Event{}, false
	}
	return ev, true
}

// view turns a State into IDs and timestamps for the API and live events.
func (s *Store) view(teamID string, st State) View {
	c := s.atlas
	v := View{TeamID: teamID, Waypoints: c.WaypointIDs(st.Waypoints), Patterns: [][]string{}, Shapes: []string{},
		Stack: []StackScanView{}, Poisoned: st.Poisoned(), CastCount: len(st.Casts), Version: st.Version,
		BossesDefeated: st.Boss.Defeated, DamageMultiplier: st.Boss.Multiplier,
		Health: st.Boss.Health, KnockedOut: st.Boss.KnockedOut(), UnlockedPoints: s.unlockedPoints(st), LockedShapes: []string{},
		CooldownShapes: []string{}, QRCooldowns: []QRCooldownView{}, Powerups: powerupsView(st.Powerups)}
	for _, qc := range st.QRCooldowns { // already soonest first, ties by waypoint index (ID order)
		v.QRCooldowns = append(v.QRCooldowns, QRCooldownView{WaypointID: c.Waypoint(qc.Waypoint).ID, Until: qc.Until.UTC()})
	}
	cooldown := s.cooldown(st)
	if cooldown != "" {
		v.CooldownClass = &cooldown
	}
	for i := range c.ShapeCount() {
		switch {
		case s.shapeLocked(i, v.UnlockedPoints):
			v.LockedShapes = append(v.LockedShapes, c.Shape(i).ID)
		case s.shapeOnCooldown(i, v.UnlockedPoints, cooldown):
			v.CooldownShapes = append(v.CooldownShapes, c.Shape(i).ID)
		}
	}
	if p := st.Boss; p.Defeated < s.bosses.Count() {
		v.Boss = &BossView{Serial: s.bosses.At(p.Defeated).Serial, Damage: p.Damage,
			PhaseStartedAt: p.PhaseStart.UTC(), Attacks: p.Attacks}
		if next, ok := p.nextAttack(s.bosses); ok {
			next = next.UTC()
			v.Boss.NextAttackAt = &next
		}
	}
	for _, p := range st.Patterns.Indexes() {
		v.Patterns = append(v.Patterns, c.WaypointIDs(c.Pattern(p).Waypoints))
	}
	for _, i := range st.Shapes.Indexes() {
		v.Shapes = append(v.Shapes, c.Shape(i).ID)
	}
	if sel := st.Selection; sel.Active {
		v.Selection = &SelectionView{Shape: c.Shape(sel.Shape).ID, SelectedBy: sel.By, SelectedAt: sel.At.UTC()}
	}
	if lc, ok := st.LastCast(); ok {
		cv := s.castView(lc)
		v.LastCast = &cv
	}
	for i := range st.Stack.Len() {
		sc := st.Stack.At(i)
		v.Stack = append(v.Stack, StackScanView{
			WaypointID: c.Waypoint(sc.Waypoint).ID, UserID: sc.UserID, ScannedAt: sc.At.UTC(), Wrong: sc.Wrong,
		})
	}
	return v
}

// powerupsView turns a team's Powerups into JSON: held and active kinds in
// kind order, both [] when empty.
func powerupsView(p Powerups) PowerupsView {
	v := PowerupsView{Held: []string{}, Active: []ActivePowerupView{}}
	for k := range powerupKinds {
		if p.Held.Has(k) {
			v.Held = append(v.Held, k.ID())
		}
	}
	if p.MultiplierArmed {
		v.Active = append(v.Active, ActivePowerupView{Kind: PowerupDamageMultiplier.ID()})
	}
	if !p.ShortQRUntil.IsZero() {
		until := p.ShortQRUntil.UTC()
		v.Active = append(v.Active, ActivePowerupView{Kind: PowerupShortQRCooldown.ID(), Until: &until})
	}
	return v
}

// castView turns a Cast into JSON, with its pattern's damage from the atlas.
func (s *Store) castView(lc Cast) CastView {
	p := s.atlas.Pattern(lc.Pattern)
	cv := CastView{Pattern: s.atlas.WaypointIDs(p.Waypoints), Shape: s.atlas.Shape(p.Shape).ID, Class: p.Spell.Class,
		CastBy: lc.By, CastAt: lc.At.UTC(), AlreadyCompleted: lc.AlreadyCompleted, Damage: p.Spell.Damage,
		Dealt: lc.Dealt, DefeatedBoss: lc.DefeatedBoss, Instant: lc.Instant, Boosted: lc.Boosted, ShortQRCooldown: lc.ShortQR}
	if lc.Boss != 0 {
		serial := lc.Boss
		cv.Boss = &serial
	}
	return cv
}

// CastsForUser returns userID's team's cast log, newest first, for
// GET /api/teams/me/casts; ErrNotInTeam if they have no team. Read from
// memory, like every other read.
func (s *Store) CastsForUser(userID uuid.UUID) ([]CastView, error) {
	t, err := s.teams.ForUser(userID)
	if err != nil {
		return nil, err
	}
	st := s.ForTeam(t.ID)
	out := make([]CastView, 0, len(st.Casts))
	for i := len(st.Casts) - 1; i >= 0; i-- {
		out = append(out, s.castView(st.Casts[i]))
	}
	return out, nil
}

// Standing is one team's place in the game, for the leaderboard: what it
// has defeated and when, and how far into its current boss it is.
type Standing struct {
	// TeamID is the team.
	TeamID string
	// BossesDefeated is how many bosses it has defeated.
	BossesDefeated int
	// LastDefeatAt is when its latest boss fell (the cast that defeated
	// it); zero if none has.
	LastDefeatAt time.Time
	// Damage is what it has dealt to its current boss so far.
	Damage int
}

// Standings returns every team with state, in no particular order: a
// lock-free load of each snapshot, so one call reads every team as it was
// at some moment, not all at the same instant.
func (s *Store) Standings() []Standing {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Standing, 0, len(s.states))
	for id, e := range s.states {
		st := e.cur.Load()
		sd := Standing{TeamID: id, BossesDefeated: st.Boss.Defeated, Damage: st.Boss.Damage}
		for i := len(st.Casts) - 1; i >= 0; i-- {
			if st.Casts[i].DefeatedBoss {
				sd.LastDefeatAt = st.Casts[i].At
				break
			}
		}
		out = append(out, sd)
	}
	return out
}
