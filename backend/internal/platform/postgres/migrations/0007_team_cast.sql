-- Every cast: each time a team's stack became exactly a pattern of its
-- selected shape. Belongs to the teamstate use case, which writes it in the
-- same transaction as the completing scan. Unlike team_pattern (one row per
-- pattern, the first completion), repeats are kept: already_completed marks a
-- pattern the team had cast before. cast_by is the member whose scan
-- completed it. The newest row per team is State.LastCast after a restart.
-- Casts from before this migration aren't in the log.
CREATE TABLE team_cast (
    id                BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    team_id           VARCHAR(6)  NOT NULL REFERENCES team (id) ON DELETE CASCADE,
    pattern_id        INTEGER     NOT NULL REFERENCES pattern (id),
    cast_by           UUID        NOT NULL REFERENCES userprofile (id),
    cast_at           TIMESTAMPTZ NOT NULL,
    already_completed BOOLEAN     NOT NULL
);

-- A team's newest cast first.
CREATE INDEX team_cast_team_id_idx ON team_cast (team_id, id DESC);
