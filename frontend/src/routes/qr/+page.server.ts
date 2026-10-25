import { apiResponse, backendFetch } from "#lib/server/backend.ts";
import { UserSchema } from "#lib/types/User.type.ts";
import { error, redirect } from "@sveltejs/kit";
import type { PageServerLoad } from "./$types";

const MeResponseSchema = apiResponse(UserSchema);

// The backend decides who's staff, from the role in the database: the
// session's role is as of sign-in and may be stale, so it isn't trusted here.
export const load: PageServerLoad = async ({ locals }) => {
    if (!locals.user || !locals.token) redirect(303, '/');

    const response = await backendFetch('/api/me', locals.token);
    if (!response.ok) throw new Error(`GET /api/me failed with ${response.status}`);
    const me = MeResponseSchema.parse(await response.json()).payload!;
    if (me.role !== 'HELPER' && me.role !== 'ADMIN') error(403, 'Staff only');
    // An admin picks any waypoint; a helper shows the one assigned to them.
    return { role: me.role };
};
