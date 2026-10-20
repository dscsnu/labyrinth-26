-- Bosses attack. Every attack_interval_s seconds after a team enters a
-- boss's phase (boss 1: when the team starts; boss n+1: the cast that
-- defeated boss n; or a revive), the boss deals attack_damage to the team's
-- health. The
-- defaults only fill rows that already exist; new bosses must give both.
ALTER TABLE boss
    ADD COLUMN attack_interval_s INTEGER NOT NULL DEFAULT 300 CHECK (attack_interval_s > 0),
    ADD COLUMN attack_damage     INTEGER NOT NULL DEFAULT 1 CHECK (attack_damage >= 0);
ALTER TABLE boss
    ALTER COLUMN attack_interval_s DROP DEFAULT,
    ALTER COLUMN attack_damage DROP DEFAULT;

-- Where a team's current boss phase began. Health isn't stored as it
-- changes: it's derived, so attacks need no write and a restart reproduces
-- them exactly. At any moment:
--   attacks = floor((now − phase_started_at) / the boss's attack_interval_s)
--   health  = max(0, phase_health − attacks × the boss's attack_damage)
-- At health 0 the team is knocked out (no selecting or scanning, no more
-- attacks) until an admin revives it. Only a boss's defeat (the next phase
-- starts at that cast) and a revive write these.
--   phase_health      the team's health when the phase began; NULL means
--                     config.toml game.health (nothing has happened yet)
--   phase_started_at  when the phase began; NULL means boss 1 from the
--                     team's start (team.started_at)
ALTER TABLE team_boss
    ADD COLUMN phase_health     INTEGER     NULL CHECK (phase_health >= 0),
    ADD COLUMN phase_started_at TIMESTAMPTZ NULL;
