-- Boss attacks move to config.toml ([game.boss_attack]: one interval for
-- every boss, and each boss's damage by serial), so the boss table no longer
-- holds them. Migration 0010's comment on team_boss still describes how
-- health is derived; the interval and damage it names now come from there.
ALTER TABLE boss
    DROP COLUMN attack_interval_s,
    DROP COLUMN attack_damage;
