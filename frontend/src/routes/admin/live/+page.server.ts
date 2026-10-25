import { apiResponse, backendFetch } from "#lib/server/backend.ts";
import { BossListSchema } from "#lib/types/Game.type.ts";
import { UserSchema } from "#lib/types/User.type.ts";
import { error, redirect } from "@sveltejs/kit";
import type { PageServerLoad } from "./$types";

const MeResponseSchema = apiResponse(UserSchema);
const BossesResponseSchema = apiResponse(BossListSchema);

// The backend decides who's an admin, from the role in the database: the
// session's role is as of sign-in and may be stale, so it isn't trusted
// here (the stream checks it again). The bosses name the serials in the feed.
export const load: PageServerLoad = async ({ locals }) => {
    if (!locals.user || !locals.token) redirect(303, '/');

    const [meRes, bossesRes] = await Promise.all([
        backendFetch('/api/me', locals.token),
        backendFetch('/api/game/bosses', locals.token),
    ]);
    if (!meRes.ok) throw new Error(`GET /api/me failed with ${meRes.status}`);
    if (MeResponseSchema.parse(await meRes.json()).payload?.role !== 'ADMIN') error(403, 'Admins only');
    if (!bossesRes.ok) throw new Error(`GET /api/game/bosses failed with ${bossesRes.status}`);
    return { bosses: BossesResponseSchema.parse(await bossesRes.json()).payload ?? [] };
};
