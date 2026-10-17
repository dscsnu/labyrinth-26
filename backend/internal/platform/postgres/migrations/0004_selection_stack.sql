-- The shape a team is attempting. Belongs to the teamstate use case, which
-- holds it in memory and writes it through.
--
-- A team's scan stack for its selection (held in memory) isn't stored
-- separately: it's the team's newest team_scan rows with id > after_scan_id,
-- at most config.toml game.max_pattern_points of them. Stale scans (older than
-- game.scan_window + game.scan_grace) are filtered out by time when used.
-- Selecting a shape sets after_scan_id to the team's latest scan, which
-- empties the stack; completing a pattern deletes the selection.
CREATE TABLE team_selection (
    team_id       VARCHAR(6)  PRIMARY KEY REFERENCES team (id) ON DELETE CASCADE,
    shape_id      TEXT        NOT NULL REFERENCES shape (id),
    selected_by   UUID        NOT NULL REFERENCES userprofile (id),
    selected_at   TIMESTAMPTZ NOT NULL,
    after_scan_id BIGINT      NOT NULL DEFAULT 0
);

-- The stack query: a team's scans newest first.
CREATE INDEX team_scan_team_id_idx ON team_scan (team_id, id DESC);
