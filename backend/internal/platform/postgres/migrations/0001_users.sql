CREATE TYPE user_role AS ENUM ('PLAYER', 'HELPER', 'ADMIN');

CREATE TABLE userprofile (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    google_sub    TEXT        NOT NULL UNIQUE,
    email         TEXT        NOT NULL UNIQUE,
    name          TEXT        NOT NULL DEFAULT '',
    avatar_url    TEXT,
    role          user_role   NOT NULL DEFAULT 'PLAYER',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One-time codes that hand a completed Google login from the backend to the
-- SvelteKit server, which redeems one for a JWT via POST /auth/token.
-- Only the SHA-256 of the code is stored.
CREATE TABLE auth_code (
    code_hash  BYTEA       PRIMARY KEY,
    user_id    UUID        NOT NULL REFERENCES userprofile (id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX auth_code_expires_at_idx ON auth_code (expires_at);
