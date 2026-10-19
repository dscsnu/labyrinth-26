import { z } from 'zod';

export const DamageRangeSchema = z.object({
    min: z.number(),
    max: z.number(),
});

// GET /api/game/shapes: what the client calls pattern types, and the timing
// rules for its countdown.
export const GameShapeSchema = z.object({
    id: z.string(),
    name: z.string(),
    patterns: z.number(),
    points: z.array(z.number()),
    damage: DamageRangeSchema,
});

export const GameSchema = z.object({
    shapes: z.array(GameShapeSchema),
    scan_window_ms: z.number(),
    scan_grace_ms: z.number(),
    max_pattern_points: z.number(),
});

// The team_state event and GET /api/teams/me/state.
export const StackScanSchema = z.object({
    waypoint_id: z.string(),
    user_id: z.string(),
    scanned_at: z.string(),
    expires_at: z.string(),
});

export const TeamStateSchema = z.object({
    team_id: z.string(),
    waypoints: z.array(z.string()),
    patterns: z.array(z.array(z.string())),
    shapes: z.array(z.string()),
    selection: z.object({ shape: z.string(), selected_by: z.string(), selected_at: z.string() }).nullable(),
    // The team's most recent cast, kept until the next one.
    last_cast: z.object({
        pattern: z.array(z.string()),
        shape: z.string(),
        cast_by: z.string(),
        cast_at: z.string(),
        already_completed: z.boolean(),
        damage: z.number(),
    }).nullable(),
    stack: z.array(StackScanSchema),
});

// POST /api/teams/me/state/scans.
export const ScanResultSchema = z.object({
    outcome: z.enum(['scan_added', 'pattern_completed']),
    pattern: z.array(z.string()).optional(),
    already_completed: z.boolean().optional(),
    state: TeamStateSchema,
});

export type DamageRange = z.infer<typeof DamageRangeSchema>;
export type GameShape = z.infer<typeof GameShapeSchema>;
export type Game = z.infer<typeof GameSchema>;
export type TeamState = z.infer<typeof TeamStateSchema>;
export type ScanResult = z.infer<typeof ScanResultSchema>;
