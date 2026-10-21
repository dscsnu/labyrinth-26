import { z } from 'zod';
import { TeamSchema } from '#lib/types/Team.type.ts';
import { UserSchema } from '#lib/types/User.type.ts';

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
});

export const TeamOverviewSchema = z.object({
    team: TeamSchema,
    game: GameOverviewSchema.nullable(),
});

export const TeamOverviewListSchema = z.array(TeamOverviewSchema);
export const UserListSchema = z.array(UserSchema);

export type GameOverview = z.infer<typeof GameOverviewSchema>;
export type TeamOverview = z.infer<typeof TeamOverviewSchema>;
