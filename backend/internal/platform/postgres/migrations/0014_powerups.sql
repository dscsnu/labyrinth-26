-- Powerups: temporary effects a team holds (at most one of each kind) and its
-- leader activates. Belongs to teamstate. One row per team and kind; no row
-- means never held or activated.
--   kind             damage_multiplier, heart, short_qr_cooldown or instant_cast
--   held             the team holds one now, waiting to be activated
--   activated_at     when one was last activated, if ever: a short_qr_cooldown
--                    stays active until activated_at + config.toml's
--                    powerups.short_qr_window
--   activated_casts  damage_multiplier only: the cast log's length when it was
--                    last activated. It's armed while the log still has that
--                    many casts, so the next cast uses it up with no write of
--                    its own.
CREATE TABLE team_powerup (
    team_id         VARCHAR(6)  NOT NULL REFERENCES team (id) ON DELETE CASCADE,
    kind            TEXT        NOT NULL CHECK (kind IN ('damage_multiplier', 'heart', 'short_qr_cooldown', 'instant_cast')),
    held            BOOLEAN     NOT NULL DEFAULT false,
    activated_at    TIMESTAMPTZ NULL,
    activated_casts INTEGER     NULL CHECK (activated_casts >= 0),
    PRIMARY KEY (team_id, kind),
    CHECK (activated_casts IS NULL OR (kind = 'damage_multiplier' AND activated_at IS NOT NULL))
);

-- What powerups did to each cast; the defaults describe every cast before 0014.
--   instant            cast by an instant_cast powerup, with no scans
--   boosted            its damage was multiplied by a damage_multiplier powerup
--   short_qr_cooldown  a short_qr_cooldown powerup was active: its waypoints
--                      cool for powerups.short_qr_cooldown, not game.qr_cooldown
ALTER TABLE team_cast
    ADD COLUMN instant           BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN boosted           BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN short_qr_cooldown BOOLEAN NOT NULL DEFAULT false;
