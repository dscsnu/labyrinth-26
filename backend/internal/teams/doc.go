// Package teams manages teams of exactly teams.size players (config.toml)
// with a leader, from forming in the lobby to starting the game. Every team
// is held in memory and every change is written through to Postgres before it
// is applied.
//
// See README.md for every rule and case, and how each file works.
package teams
