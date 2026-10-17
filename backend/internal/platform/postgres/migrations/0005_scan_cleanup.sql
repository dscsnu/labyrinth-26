-- team_scan_team_id_idx (team_id, id DESC), from 0004, serves every query on
-- a team's scans, so 0003's (team_id) index is redundant.
DROP INDEX team_scan_team_idx;

-- 0003's comment above team_scan says "every scan, including repeats". Since
-- 0004 only accepted scans are written: rejected scans (rescans, a member's
-- second scan, waypoints outside the pattern) never are.
COMMENT ON TABLE team_scan IS
    'One row per accepted scan (rejected scans are never written): the audit trail, and the source of each team''s scan stack after a restart.';
