-- A helper's assigned checkpoint: the waypoint whose rotating QR their /qr
-- display shows (they can't pick one; an admin assigns it). NULL means none.
-- Only staff can hold one, so demoting a user to PLAYER must clear it (the
-- user use case does, in the same UPDATE). Several helpers may share a
-- waypoint (shifts). A waypoint the seed removes unassigns its helpers.
ALTER TABLE userprofile
    ADD COLUMN waypoint_id TEXT NULL REFERENCES waypoint (id) ON DELETE SET NULL,
    ADD CONSTRAINT userprofile_waypoint_staff CHECK (waypoint_id IS NULL OR role <> 'PLAYER');

COMMENT ON COLUMN userprofile.waypoint_id IS 'The checkpoint a helper''s /qr display shows, assigned by an admin; NULL: none. Staff only (userprofile_waypoint_staff).';
