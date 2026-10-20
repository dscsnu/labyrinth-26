-- Spell classes for local development: one per pattern size the game allows
-- (game.max_pattern_points is at most 15), and every pattern put in the class
-- of its waypoint count. Idempotent; applied by `go run . seed` after the
-- patterns, with the backend stopped.

BEGIN;

INSERT INTO spell_class (id, name, points)
SELECT 'class_' || n, 'Class ' || n, n
FROM generate_series(1, 15) AS n
ON CONFLICT (id) DO NOTHING;

UPDATE pattern p
SET spell_class_id = sc.id
FROM (SELECT pattern_id, count(*) AS n FROM pattern_waypoint GROUP BY pattern_id) AS c
JOIN spell_class sc ON sc.points = c.n
WHERE c.pattern_id = p.id
  AND p.spell_class_id IS DISTINCT FROM sc.id;

COMMIT;
