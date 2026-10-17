-- Static game content. Loaded into memory at startup; edit it only while the
-- backend is stopped.

-- Waypoints are physical locations with a QR code. id is a fixed 4-digit
-- code (e.g. '1001', leading zeros allowed) so printed codes and stored
-- progress never change meaning.
CREATE TABLE waypoint (
    id        TEXT             PRIMARY KEY CHECK (id ~ '^[0-9]{4}$'),
    name      TEXT             NOT NULL,
    latitude  DOUBLE PRECISION NOT NULL,
    longitude DOUBLE PRECISION NOT NULL
);

CREATE TABLE shape (
    id   TEXT PRIMARY KEY CHECK (id ~ '^[a-z0-9_-]{1,32}$'),
    name TEXT NOT NULL
);

-- A pattern is one concrete set of waypoints that forms a shape on the map.
-- It is identified by that set; id is only an internal key.
CREATE TABLE pattern (
    id       INTEGER PRIMARY KEY,
    shape_id TEXT    NOT NULL REFERENCES shape (id)
);

CREATE TABLE pattern_waypoint (
    pattern_id  INTEGER NOT NULL REFERENCES pattern (id) ON DELETE CASCADE,
    waypoint_id TEXT    NOT NULL REFERENCES waypoint (id),
    PRIMARY KEY (pattern_id, waypoint_id)
);

-- Team progress, written through from memory.

-- Every scan, including repeats: the audit trail for disputes.
CREATE TABLE team_scan (
    id          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    team_id     VARCHAR(6)  NOT NULL REFERENCES team (id) ON DELETE CASCADE,
    waypoint_id TEXT        NOT NULL REFERENCES waypoint (id),
    user_id     UUID        NOT NULL REFERENCES userprofile (id),
    scanned_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX team_scan_team_idx ON team_scan (team_id);

CREATE TABLE team_pattern (
    team_id      VARCHAR(6)  NOT NULL REFERENCES team (id) ON DELETE CASCADE,
    pattern_id   INTEGER     NOT NULL REFERENCES pattern (id),
    completed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, pattern_id)
);
