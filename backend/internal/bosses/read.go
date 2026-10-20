package bosses

import "time"

// Boss is one boss, as GET /api/game/bosses shows it.
type Boss struct {
	// Serial is its place in the sequence: 1 is fought first.
	Serial int `json:"serial"`
	// Name is for display, e.g. "Matthew Patel".
	Name string `json:"name"`
	// Asset is a key the frontend resolves to the boss's image, e.g.
	// "bosses/1.png"; not a URL.
	Asset string `json:"asset"`
	// Description is for display.
	Description string `json:"description"`
	// HP is the damage a team must deal to defeat it.
	HP int `json:"hp"`
	// AttackIntervalS is how often it attacks, in seconds: every
	// AttackIntervalS of a team's phase against it. From config.toml
	// (game.boss_attack.interval, the same for every boss), not the table.
	AttackIntervalS int `json:"attack_interval_s"`
	// AttackDamage is what each attack takes off the team's health. From
	// config.toml (game.boss_attack.damage, by serial), not the table.
	AttackDamage int `json:"attack_damage"`
}

// AttackInterval is AttackIntervalS as a Duration.
func (b Boss) AttackInterval() time.Duration { return time.Duration(b.AttackIntervalS) * time.Second }

// Bosses is the sequence every team fights, in memory. Read-only after New,
// so safe for concurrent use without locks.
type Bosses struct {
	// list is every boss; list[i] has serial i+1.
	list []Boss
}

// Count returns how many bosses there are: game.bosses, or 0 if none are
// stored yet.
func (b *Bosses) Count() int { return len(b.list) }

// At returns the boss at index i (serial i+1), 0 ≤ i < Count. It panics on
// an index out of range: callers index by a team's progress, which never
// passes Count.
func (b *Bosses) At(i int) Boss { return b.list[i] }

// All returns every boss in serial order, for GET /api/game/bosses. The
// slice is the store's own; callers must not modify it.
func (b *Bosses) All() []Boss { return b.list }
