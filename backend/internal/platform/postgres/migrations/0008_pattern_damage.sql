-- Migration 0008: add damage column to pattern table.
-- Stored once in Postgres and loaded into memory at startup (atlas.Load).
-- Damage must be positive (> 0).
ALTER TABLE pattern ADD COLUMN damage INTEGER CHECK (damage > 0);
