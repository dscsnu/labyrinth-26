import { apiResponse, backendFetch } from "#lib/server/backend.ts";
import { LeaderboardMineSchema } from "#lib/types/Leaderboard.type.ts";
import { redirect } from "@sveltejs/kit";
import type { PageServerLoad } from "./$types";

const LeaderboardResponseSchema = apiResponse(LeaderboardMineSchema);

// The leaderboard as it is now, with the signed-in player's own team; the
// page then follows the game-wide "leaderboard" event for the top.
export const load: PageServerLoad = async ({ locals }) => {
    if (!locals.user || !locals.token) redirect(303, '/');

    const response = await backendFetch('/api/leaderboard', locals.token);
    if (!response.ok) throw new Error(`GET /api/leaderboard failed with ${response.status}`);
    return { board: LeaderboardResponseSchema.parse(await response.json()).payload! };
}
