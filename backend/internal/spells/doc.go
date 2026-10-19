// Package spells provides offline mathematical modeling and damage calculation
// for Labyrinth patterns: equirectangular campus projection, shortest closed
// tour (TSP), Andrew's monotone chain convex hull, and calibrated spell damage.
// It is used by offline generators (cmd/spells) and tests, and is never
// imported by the runtime game server.
//
// See README.md for every rule and case, and how each file works.
package spells
