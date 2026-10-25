import { z } from 'zod';
import { VersionSchema } from '#lib/types/Live.type.ts';

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
    // Its patterns counted by size ({"3": 12, "4": 6}); a team casts those of
    // at most its team_state.unlocked_points.
    patterns_by_points: z.record(z.string(), z.number()),
});

export const GameSchema = z.object({
    shapes: z.array(GameShapeSchema),
    max_pattern_points: z.number(),
    // game.health: every team's starting health.
    health: z.number(),
    // The unlock schedule (game.unlocks), ascending: once a team has
    // defeated after_bosses bosses, it can cast patterns of up to points
    // waypoints. The team's current limit is team_state.unlocked_points.
    unlocks: z.array(z.object({ after_bosses: z.number(), points: z.number() })),
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
    // The pattern's damage, and what it dealt to the boss (× the team's
    // multiplier then, rounded; 0 once every boss is defeated).
    damage: z.number(),
    dealt: z.number(),
    // The serial of the boss it hit, or null if none.
    boss: z.number().nullable(),
    defeated_boss: z.boolean(),
    // Its spell class, e.g. "class_3": the class of patterns of its size.
    class: z.string(),
    // What powerups did to it: cast by instant_cast with no scans; dealt
    // multiplied by damage_multiplier; its waypoints cooled for
    // powerups.short_qr_cooldown instead of game.qr_cooldown.
    instant: z.boolean(),
    boosted: z.boolean(),
    short_qr_cooldown: z.boolean(),
});

// GET /api/game/bosses: every boss, serial 1 first. Static.
export const BossSchema = z.object({
    serial: z.number(),
    name: z.string(),
    // A key for the boss's image under static/, e.g. "bosses/1.png".
    asset: z.string(),
    description: z.string(),
    hp: z.number(),
    // Every attack_interval_s seconds of a team's phase against it, the boss
    // takes attack_damage off the team's health.
    attack_interval_s: z.number(),
    attack_damage: z.number(),
});
export const BossListSchema = z.array(BossSchema);

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
    // Progress through the bosses: the current one (by serial, with the
    // damage dealt to it so far), or null once every boss is defeated.
    bosses_defeated: z.number(),
    boss: z.object({
        serial: z.number(),
        damage: z.number(),
        // Its attack clock: when the phase began, attacks so far, and when it
        // attacks next (null once the team is knocked out).
        phase_started_at: z.string(),
        attacks: z.number(),
        next_attack_at: z.string().nullable(),
    }).nullable(),
    damage_multiplier: z.number(),
    // game.health less every boss attack; at 0 the team is knocked out
    // (selecting and scanning refused) until an admin revives it.
    health: z.number(),
    knocked_out: z.boolean(),
    // The biggest pattern the team can cast now, and the types it can't
    // select yet (all their patterns are bigger).
    unlocked_points: z.number(),
    locked_shapes: z.array(z.string()),
    // The class of the team's last cast, which it can't cast again next
    // (null before the first cast), and the types that leaves it unable to
    // select (not in locked_shapes).
    cooldown_class: z.string().nullable(),
    cooldown_shapes: z.array(z.string()),
    // Waypoints the team can't scan yet (409 waypoint_on_cooldown): those of
    // its recent casts, each with when it opens again. Soonest first.
    qr_cooldowns: z.array(z.object({ waypoint_id: z.string(), until: z.string() })),
    // How many casts the log holds; more than the client's copy means it
    // missed some and should re-fetch GET /api/teams/me/casts.
    cast_count: z.number(),
    // The powerups the team holds, and the effects running (until null:
    // used up by the next cast).
    powerups: z.object({
        held: z.array(z.string()),
        active: z.array(z.object({ kind: z.string(), until: z.string().nullable() })),
    }),
    // The team's latest quick-time-event answer, or null if it never
    // answered one.
    qte: z.object({
        qte_id: z.number(),
        option: z.number(),
        correct: z.boolean(),
        granted: z.boolean(),
        answered_by: z.string(),
        answered_at: z.string(),
    }).nullable(),
    version: VersionSchema,
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

// The game-wide "qte" event and GET /api/qte: the open quick-time event, or
// the one just closed (with its answer), or null when there's none.
export const QTESchema = z.object({
    id: z.number(),
    kind: z.string(),
    status: z.enum(['open', 'closed']),
    // The powerup kind a right answer earns.
    powerup: z.string(),
    started_at: z.string(),
    ends_at: z.string(),
    quiz: z.object({
        question: z.string(),
        options: z.array(z.string()),
        // The right option's index once closed; null while open.
        correct_option: z.number().nullable(),
    }).optional(),
});

export type QTE = z.infer<typeof QTESchema>;
export type DamageRange = z.infer<typeof DamageRangeSchema>;
export type GameShape = z.infer<typeof GameShapeSchema>;
export type Game = z.infer<typeof GameSchema>;
export type Cast = z.infer<typeof CastSchema>;
export type Boss = z.infer<typeof BossSchema>;
export type TeamState = z.infer<typeof TeamStateSchema>;
export type ScanResult = z.infer<typeof ScanResultSchema>;
