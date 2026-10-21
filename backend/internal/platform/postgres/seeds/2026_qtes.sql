-- Placeholder quizzes, one per powerup, on the bosses' (2025) theme until
-- the 2026 questions are written: drafts for an admin to start (POST
-- /api/admin/qtes/{id}/start), at fixed ids 1-4 so the same ids work on
-- every database. Players never see a draft. Idempotent: a
-- rerun refreshes a seeded draft's content but never touches a QTE that has
-- run, or one an admin wrote (created_by set) that took one of these ids.
-- The id sequence then moves past them, so admin-written QTEs never collide.
INSERT INTO qte (id, kind, powerup, question, options, correct_option) VALUES
    (1, 'quiz', 'heart', 'How many evil exes does Scott have to defeat?',
        ARRAY['Five', 'Six', 'Seven', 'Eight'], 2),
    (2, 'quiz', 'damage_multiplier', 'Which band does Scott play bass in?',
        ARRAY['Sex Bob-omb', 'Crash and the Boys', 'The Clash at Demonhead', 'Kid Chocolate'], 0),
    (3, 'quiz', 'short_qr_cooldown', 'What is Ramona''s job?',
        ARRAY['Courier', 'Barista', 'Librarian', 'DJ'], 0),
    (4, 'quiz', 'instant_cast', 'Where do Todd Ingram''s psychic powers come from?',
        ARRAY['Being vegan', 'Caffeine', 'His bass', 'Meditation'], 0)
ON CONFLICT (id) DO UPDATE SET
    kind = EXCLUDED.kind, powerup = EXCLUDED.powerup, question = EXCLUDED.question,
    options = EXCLUDED.options, correct_option = EXCLUDED.correct_option
    WHERE qte.started_at IS NULL AND qte.created_by IS NULL;
SELECT setval(pg_get_serial_sequence('qte', 'id'), (SELECT max(id) FROM qte));
