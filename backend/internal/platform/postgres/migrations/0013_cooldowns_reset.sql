-- An admin can reset a team's cooldowns (the spell class and QR cooldowns,
-- both derived from the cast log). cooldowns_reset_casts is how many casts
-- the team's log held at the last reset: only casts after those count for
-- cooldowns. A position in the log, not a time, so a cast made the same
-- instant as the reset still counts. 0 means never reset. On team_boss,
-- teamstate's one per-team progress row.
ALTER TABLE team_boss ADD COLUMN cooldowns_reset_casts INTEGER NOT NULL DEFAULT 0 CHECK (cooldowns_reset_casts >= 0);
