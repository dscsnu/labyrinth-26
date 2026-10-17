CREATE TYPE team_status AS ENUM ('LOBBY', 'IN_GAME', 'FINISHED');

CREATE TABLE team (
    id         VARCHAR(6)  PRIMARY KEY CHECK (id ~ '^[0-9]{6}$'),
    name       TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 32),
    leader_id  UUID        NOT NULL REFERENCES userprofile (id),
    status     team_status NOT NULL DEFAULT 'LOBBY',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ
);

-- user_id is unique: a user is in at most one team.
CREATE TABLE teammember (
    team_id   VARCHAR(6)  NOT NULL REFERENCES team (id) ON DELETE CASCADE,
    user_id   UUID        NOT NULL UNIQUE REFERENCES userprofile (id) ON DELETE CASCADE,
    is_ready  BOOLEAN     NOT NULL DEFAULT false,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_id)
);
