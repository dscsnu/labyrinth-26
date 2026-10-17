import { apiResponse, backendFetch } from "#lib/server/backend.ts";
import { GameSchema, TeamStateSchema } from "#lib/types/Game.type.ts";
import { redirect } from "@sveltejs/kit";
import type { PageServerLoad } from "./$types";

const GameResponseSchema = apiResponse(GameSchema);
const StateResponseSchema = apiResponse(TeamStateSchema);

export const load: PageServerLoad = async ({ locals }) => {
    if (!locals.user || !locals.token) redirect(303, '/');

    const [gameRes, stateRes] = await Promise.all([
        backendFetch('/api/game/shapes', locals.token),
        backendFetch('/api/teams/me/state', locals.token),
    ]);
    if (!gameRes.ok) throw new Error(`GET /api/game/shapes failed with ${gameRes.status}`);
    if (stateRes.status === 404) redirect(303, '/team'); // not in a team
    if (!stateRes.ok) throw new Error(`GET /api/teams/me/state failed with ${stateRes.status}`);

    return {
        game: GameResponseSchema.parse(await gameRes.json()).payload!,
        state: StateResponseSchema.parse(await stateRes.json()).payload!,
    };
}
