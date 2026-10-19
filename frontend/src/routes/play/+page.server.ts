import { apiResponse, backendFetch } from "#lib/server/backend.ts";
import { BossListSchema, CastLogSchema, GameSchema, TeamStateSchema } from "#lib/types/Game.type.ts";
import { redirect } from "@sveltejs/kit";
import type { PageServerLoad } from "./$types";

const GameResponseSchema = apiResponse(GameSchema);
const StateResponseSchema = apiResponse(TeamStateSchema);
const CastsResponseSchema = apiResponse(CastLogSchema);
const BossesResponseSchema = apiResponse(BossListSchema);

export const load: PageServerLoad = async ({ locals }) => {
    if (!locals.user || !locals.token) redirect(303, '/');

    const [gameRes, stateRes, castsRes, bossesRes] = await Promise.all([
        backendFetch('/api/game/shapes', locals.token),
        backendFetch('/api/teams/me/state', locals.token),
        backendFetch('/api/teams/me/casts', locals.token),
        backendFetch('/api/game/bosses', locals.token),
    ]);
    if (!gameRes.ok) throw new Error(`GET /api/game/shapes failed with ${gameRes.status}`);
    if (stateRes.status === 404) redirect(303, '/team'); // not in a team
    if (!stateRes.ok) throw new Error(`GET /api/teams/me/state failed with ${stateRes.status}`);
    if (!castsRes.ok) throw new Error(`GET /api/teams/me/casts failed with ${castsRes.status}`);
    if (!bossesRes.ok) throw new Error(`GET /api/game/bosses failed with ${bossesRes.status}`);

    return {
        game: GameResponseSchema.parse(await gameRes.json()).payload!,
        state: StateResponseSchema.parse(await stateRes.json()).payload!,
        // The cast log at load, newest first; the page prepends each new
        // last_cast it sees.
        casts: CastsResponseSchema.parse(await castsRes.json()).payload!,
        // Every boss in serial order (static); the team's progress is in its state.
        bosses: BossesResponseSchema.parse(await bossesRes.json()).payload!,
    };
}
