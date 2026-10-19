-- Bosses: the sequence every team fights, in serial order (1, 2, …). Static
-- content, read once at startup by the bosses use case, like the atlas
-- tables. config.toml game.bosses is how many there are: startup refuses a
-- table that isn't exactly serials 1…game.bosses. asset is a key the
-- frontend resolves to an image (e.g. "bosses/1.png"), not a URL.
CREATE TABLE boss (
    serial      INTEGER PRIMARY KEY CHECK (serial >= 1),
    name        TEXT    NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
    asset       TEXT    NOT NULL CHECK (asset <> ''),
    description TEXT    NOT NULL,
    hp          INTEGER NOT NULL CHECK (hp > 0)
);

-- Each team's progress through the bosses. Belongs to the teamstate use case,
-- which writes it in the same transaction as the cast that changed it. No row
-- means no progress: boss 1, no damage, multiplier 1.
--   defeated    how many bosses the team has defeated; it's fighting boss
--               serial defeated + 1, or none once defeated = game.bosses
--   damage      damage dealt to the current boss so far (always < its hp
--               after a cast: a cast reaching hp defeats it, and the rest of
--               that cast's damage is lost)
--   multiplier  what each cast's damage is multiplied by (rounded to an
--               integer) before it's dealt; 1 unless something sets it
CREATE TABLE team_boss (
    team_id    VARCHAR(6)       PRIMARY KEY REFERENCES team (id) ON DELETE CASCADE,
    defeated   INTEGER          NOT NULL DEFAULT 0 CHECK (defeated >= 0),
    damage     INTEGER          NOT NULL DEFAULT 0 CHECK (damage >= 0),
    multiplier DOUBLE PRECISION NOT NULL DEFAULT 1 CHECK (multiplier > 0)
);

-- What each cast did to the bosses: dealt is the damage it dealt (the
-- pattern's damage × the multiplier, rounded), boss_serial the boss it hit
-- (NULL once every boss is defeated, or for casts from before this
-- migration), defeated_boss whether it finished that boss.
ALTER TABLE team_cast
    ADD COLUMN dealt         INTEGER NOT NULL DEFAULT 0 CHECK (dealt >= 0),
    ADD COLUMN boss_serial   INTEGER NULL REFERENCES boss (serial),
    ADD COLUMN defeated_boss BOOLEAN NOT NULL DEFAULT false;
