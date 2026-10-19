import { z } from 'zod';

export const DamageRangeSchema = z.object({
    min: z.number(),
    max: z.number(),
});

// GET /api/game/shapes: what the client calls pattern types. There's no time
// limit, so no timing rules.
export const GameShapeSchema = z.object({
    id: z.string(),
    name: z.string(),
    patterns: z.number(),
    points: z.array(z.number()),
    damage: DamageRangeSchema,
});

export const GameSchema = z.object({
    shapes: z.array(GameShapeSchema),
    max_pattern_points: z.number(),
});

// The team_state event and GET /api/teams/me/state.
export const StackScanSchema = z.object({
    waypoint_id: z.string(),
    user_id: z.string(),
    scanned_at: z.string(),
    // The scan didn't fit any pattern of the selected type: the attempt is
    // poisoned until someone selects again.
    wrong: z.boolean(),
});

// One completed pattern: last_cast, and each entry of GET /api/teams/me/casts.
export const CastSchema = z.object({
    pattern: z.array(z.string()),
    shape: z.string(),
    cast_by: z.string(),
    cast_at: z.string(),
    already_completed: z.boolean(),
    damage: z.number(),
});

export const TeamStateSchema = z.object({
    team_id: z.string(),
    waypoints: z.array(z.string()),
    patterns: z.array(z.array(z.string())),
    shapes: z.array(z.string()),
    selection: z.object({ shape: z.string(), selected_by: z.string(), selected_at: z.string() }).nullable(),
    // The team's most recent cast, kept until the next one.
    last_cast: CastSchema.nullable(),
    stack: z.array(StackScanSchema),
    // A wrong scan is in the stack: scans are refused until a reselect.
    poisoned: z.boolean(),
});

// GET /api/teams/me/casts: every cast, newest first.
export const CastLogSchema = z.array(CastSchema);

// POST /api/teams/me/state/scans.
export const ScanResultSchema = z.object({
    outcome: z.enum(['scan_added', 'wrong_scan', 'pattern_completed']),
    pattern: z.array(z.string()).optional(),
    already_completed: z.boolean().optional(),
    state: TeamStateSchema,
});

export type DamageRange = z.infer<typeof DamageRangeSchema>;
export type GameShape = z.infer<typeof GameShapeSchema>;
export type Game = z.infer<typeof GameSchema>;
export type Cast = z.infer<typeof CastSchema>;
export type TeamState = z.infer<typeof TeamStateSchema>;
export type ScanResult = z.infer<typeof ScanResultSchema>;
