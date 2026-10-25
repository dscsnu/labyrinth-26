import { apiResponse, backendFetch } from "#lib/server/backend.ts";
import { SigningKeySchema } from "#lib/types/Qr.type.ts";
import { UserSchema } from "#lib/types/User.type.ts";
import { error, redirect } from "@sveltejs/kit";
import type { PageServerLoad } from "./$types";

const MeResponseSchema = apiResponse(UserSchema);
const KeyResponseSchema = apiResponse(SigningKeySchema);

// The backend decides who's staff, from the role in the database: the
// session's role is as of sign-in and may be stale, so it isn't trusted here.
export const load: PageServerLoad = async ({ locals }) => {
    if (!locals.user || !locals.token) redirect(303, '/');

    const response = await backendFetch('/api/me', locals.token);
    if (!response.ok) throw new Error(`GET /api/me failed with ${response.status}`);
    const me = MeResponseSchema.parse(await response.json()).payload!;
    if (me.role !== 'HELPER' && me.role !== 'ADMIN') error(403, 'Staff only');
    // An admin picks any waypoint. A helper shows the one assigned to them,
    // read once here: the page never asks again, so a new assignment shows on
    // the next refresh (no polling: the server is busy enough on game day).
    if (me.role === 'ADMIN') return { role: me.role, assigned: null };
    const keyRes = await backendFetch('/api/qr/me', locals.token);
    if (keyRes.status === 404) return { role: me.role, assigned: null }; // not_assigned
    if (!keyRes.ok) throw new Error(`GET /api/qr/me failed with ${keyRes.status}`);
    return { role: me.role, assigned: KeyResponseSchema.parse(await keyRes.json()).payload! };
};
