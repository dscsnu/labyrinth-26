-- Spell classes group patterns by how many waypoints they have: every
-- pattern of n waypoints is in the class whose points is n (class_4 for a
-- 4-point pattern, whatever its shape). Static content like shape: written by
-- seeds with the backend stopped, read once at startup by atlas.Load, which
-- refuses a pattern without a class or whose class's points isn't its
-- waypoint count.
CREATE TABLE spell_class (
    -- The stable ID clients see, e.g. 'class_4'.
    id     TEXT     PRIMARY KEY CHECK (id ~ '^[a-z0-9_-]{1,32}$'),
    -- For display, e.g. 'Class 4'.
    name   TEXT     NOT NULL,
    -- The waypoint count of its patterns: one class per count.
    points SMALLINT NOT NULL UNIQUE CHECK (points BETWEEN 1 AND 64)
);

-- Nullable so the migration applies to a database that already has patterns;
-- the seed fills it, and atlas.Load refuses a pattern left without one.
ALTER TABLE pattern ADD COLUMN spell_class_id TEXT REFERENCES spell_class (id);
