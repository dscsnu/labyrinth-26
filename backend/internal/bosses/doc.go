// Package bosses is the sequence of bosses every team fights: each one's
// name, image asset, description and HP, in serial order (1, 2, …). It's
// static content, loaded from Postgres once at startup, validated against
// config.toml game.bosses, and held in memory; nothing changes it while the
// backend runs, so every lookup is lock-free. A team's progress through them
// (which boss, how much damage) is per-team state and belongs to teamstate,
// which reads this package through a small interface of its own.
//
// See README.md for every rule and case, and how each file works.
package bosses
