-- A scan that doesn't fit the selected shape's patterns is still recorded
-- (it occupies the member's one scan and its waypoint slot, and poisons the
-- selection until the team reselects). wrong marks it so a restart can
-- reproduce the stack and keep it from aging out like a normal scan.
ALTER TABLE team_scan ADD COLUMN wrong BOOLEAN NOT NULL DEFAULT false;
