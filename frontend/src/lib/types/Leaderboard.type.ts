import { z } from 'zod';

// One team's place: ranked by bosses defeated, then the earliest last
// defeat, then damage on the current boss.
export const LeaderboardEntrySchema = z.object({
    rank: z.number(),
    team_id: z.string(),
    name: z.string(),
    bosses_defeated: z.number(),
    last_defeat_at: z.string().nullable(),
    damage: z.number(),
    finished: z.boolean(),
});

// The "leaderboard" event's message: the top leaderboard.size teams.
export const LeaderboardBoardSchema = z.object({
    top: z.array(LeaderboardEntrySchema),
    teams: z.number(),
});

// GET /api/leaderboard: the top, plus the caller's own team (null without one).
export const LeaderboardMineSchema = LeaderboardBoardSchema.extend({
    me: LeaderboardEntrySchema.nullable(),
});

export type LeaderboardEntry = z.infer<typeof LeaderboardEntrySchema>;
export type LeaderboardBoard = z.infer<typeof LeaderboardBoardSchema>;
