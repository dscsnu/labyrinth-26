import { z } from 'zod';
import { TeamSchema } from '#lib/types/Team.type.ts';
import { UserSchema } from '#lib/types/User.type.ts';
import { LeaderboardEntrySchema } from '#lib/types/Leaderboard.type.ts';

// GET /api/admin/teams: each team, and for a started one its game at a
// glance: who needs a revive, where it is, what's cooling down.
export const GameOverviewSchema = z.object({
    health: z.number(),
    knocked_out: z.boolean(),
    bosses_defeated: z.number(),
    boss: z.object({ serial: z.number(), damage: z.number(), next_attack_at: z.string().nullable() }).nullable(),
    cooldown_class: z.string().nullable(),
    qr_cooldowns: z.array(z.object({ waypoint_id: z.string(), until: z.string() })),
    cast_count: z.number(),
    // Held: waiting for the leader; active: running (until null: the next cast).
    powerups: z.object({
        held: z.array(z.string()),
        active: z.array(z.object({ kind: z.string(), until: z.string().nullable() })),
    }),
});

export const TeamOverviewSchema = z.object({
    team: TeamSchema,
    game: GameOverviewSchema.nullable(),
});

export const TeamOverviewListSchema = z.array(TeamOverviewSchema);
export const UserListSchema = z.array(UserSchema);

// GET /api/admin/qtes: every quiz, newest first, with its answer, its status
// and how the teams answered.
export const AdminQTESchema = z.object({
    id: z.number(),
    powerup: z.string(),
    quiz: z.object({ question: z.string(), options: z.array(z.string()), correct_option: z.number() }).nullable(),
    window_s: z.number().nullable(),
    started_at: z.string().nullable(),
    ends_at: z.string().nullable(),
    status: z.enum(['draft', 'open', 'closed']),
    stats: z.object({ answered: z.number(), correct: z.number(), granted: z.number() }),
});
export const AdminQTEListSchema = z.array(AdminQTESchema);

export type GameOverview = z.infer<typeof GameOverviewSchema>;
export type TeamOverview = z.infer<typeof TeamOverviewSchema>;
export type AdminQTE = z.infer<typeof AdminQTESchema>;

// The admin live view (/admin/live), on GET /api/events?admin=1.
// One change to one team's game state; only its type's fields are set.
export const ActivityEntrySchema = z.object({
    at: z.string(),
    team_id: z.string(),
    team: z.string(),
    type: z.string(),
    by: z.string().nullable(),
    shape: z.string().optional(),
    waypoint: z.string().optional(),
    dealt: z.number().optional(),
    boss: z.number().optional(),
    health: z.number().optional(),
    powerup: z.string().optional(),
    correct: z.boolean().optional(),
});
// The "activity" event's message: entries oldest first, and how many more
// the server left out of a batch.
export const ActivityBatchSchema = z.object({
    entries: z.array(ActivityEntrySchema),
    dropped: z.number(),
});
// The "admin_leaderboard" event's message: every started team, ranked.
export const AdminLeaderboardSchema = z.object({
    teams: z.array(LeaderboardEntrySchema),
});

export type ActivityEntry = z.infer<typeof ActivityEntrySchema>;
