-- 2026 content: the 10 checkpoints on campus, and every combination of 2 to
-- 6 of them as a pattern (837), each shaped by its size: line (2),
-- triangle (3), quadrilateral (4), pentagon (5), hexagon (6). Coordinates
-- and names are damage_lookup.json's (repository root, the 2026 source of
-- truth), numbered 2001-2010 in its order. Damage comes after, from
-- seeds/2026_spells.sql (go run ./cmd/spells).
--
-- Pattern IDs are stable: every combination ordered by size, then by its
-- sorted waypoint IDs, numbered from 1 (2001-2002 is 1, … 2005-…-2010 is
-- 837). Spell classes are one per size (class_2 … class_6; class_1 … class_15
-- exist, for any game.max_pattern_points).
--
-- It replaces whatever content the database held (the 2025 placeholders
-- included): patterns, shapes and waypoints not in this file are deleted.
-- It refuses to when teams have progress on other content (their scans,
-- casts and selections would point at deleted or renumbered content): reset
-- the database first. Idempotent; apply with `go run . seed` while the
-- backend is stopped.

BEGIN;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM waypoint WHERE id NOT BETWEEN '2001' AND '2010')
       AND (EXISTS (SELECT 1 FROM team_scan) OR EXISTS (SELECT 1 FROM team_cast) OR EXISTS (SELECT 1 FROM team_selection)) THEN
        RAISE EXCEPTION 'the database has team progress on other content; reset it (docker compose down -v) before seeding 2026';
    END IF;
END $$;

INSERT INTO waypoint (id, name, latitude, longitude) VALUES
    ('2001', 'G Block Junc', 28.5275017, 77.5748206),
    ('2002', 'Arcade', 28.5274593, 77.5728646),
    ('2003', 'Inner gate', 28.5255101, 77.5716509),
    ('2004', 'Chess Garden', 28.5238914, 77.5720264),
    ('2005', 'Amphi', 28.5238216, 77.5740059),
    ('2006', 'AIC', 28.5249946, 77.5737913),
    ('2007', 'Lake', 28.524414, 77.5768289),
    ('2008', 'Canopies', 28.5263552, 77.5743113),
    ('2009', 'A Block (A)', 28.52692140098942, 77.57706407672785),
    ('2010', 'CnD Atrium (CnD)', 28.525519393291614, 77.57653461322104)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, latitude = EXCLUDED.latitude, longitude = EXCLUDED.longitude;

INSERT INTO shape (id, name) VALUES
    ('line', 'Line'),
    ('triangle', 'Triangle'),
    ('quadrilateral', 'Quadrilateral'),
    ('pentagon', 'Pentagon'),
    ('hexagon', 'Hexagon')
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name;

INSERT INTO spell_class (id, name, points)
SELECT 'class_' || n, 'Class ' || n, n
FROM generate_series(1, 15) AS n
ON CONFLICT (id) DO NOTHING;

-- Every combination of 2 to 6 of the 10 waypoints, with its stable ID.
CREATE TEMP TABLE combos ON COMMIT DROP AS
WITH RECURSIVE c (ids) AS (
    SELECT ARRAY[id] FROM waypoint WHERE id BETWEEN '2001' AND '2010'
    UNION ALL
    SELECT c.ids || w.id
    FROM c JOIN waypoint w ON w.id > c.ids[cardinality(c.ids)] AND w.id BETWEEN '2001' AND '2010'
    WHERE cardinality(c.ids) < 6
)
SELECT row_number() OVER (ORDER BY cardinality(ids), ids)::int AS id, ids,
       (ARRAY['line', 'triangle', 'quadrilateral', 'pentagon', 'hexagon'])[cardinality(ids) - 1] AS shape_id,
       'class_' || cardinality(ids) AS spell_class_id
FROM c
WHERE cardinality(ids) >= 2;

DELETE FROM pattern_waypoint;
DELETE FROM pattern WHERE id NOT IN (SELECT id FROM combos);
INSERT INTO pattern (id, shape_id, spell_class_id)
SELECT id, shape_id, spell_class_id FROM combos
ON CONFLICT (id) DO UPDATE SET shape_id = EXCLUDED.shape_id, spell_class_id = EXCLUDED.spell_class_id;
INSERT INTO pattern_waypoint (pattern_id, waypoint_id)
SELECT id, unnest(ids) FROM combos;

DELETE FROM shape WHERE id NOT IN ('line', 'triangle', 'quadrilateral', 'pentagon', 'hexagon');
DELETE FROM waypoint WHERE id NOT BETWEEN '2001' AND '2010';

COMMIT;
