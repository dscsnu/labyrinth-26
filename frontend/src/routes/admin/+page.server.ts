import { apiResponse, backendFetch } from "#lib/server/backend.ts";
import { TeamSchema } from "#lib/types/Team.type.ts";
import { error, fail, redirect } from "@sveltejs/kit";
import { z } from "zod";
import type { Actions, PageServerLoad } from "./$types";

const TeamsResponseSchema = apiResponse(z.array(TeamSchema));

// The backend decides who's an admin, from the role in the database: the
// session's role is as of sign-in and may be stale, so it isn't trusted here.
export const load: PageServerLoad = async ({ locals }) => {
    if (!locals.user || !locals.token) redirect(303, '/');

    const response = await backendFetch('/api/admin/teams', locals.token);
    if (response.status === 403) error(403, 'Admins only');
    if (!response.ok) throw new Error(`GET /api/admin/teams failed with ${response.status}`);

    return { teams: TeamsResponseSchema.parse(await response.json()).payload ?? [] };
}

// Calls an admin route; errors come back as form data with the backend's
// code and message.
const adminCall = async (token: string | null, path: string, method: string) => {
    if (!token) redirect(303, '/');
    const response = await backendFetch(path, token, { method });
    if (response.ok) return null;
    const data = await response.json().catch(() => ({}));
    return fail(response.status, {
        code: (data.code as string | undefined) ?? 'error',
        message: (data.message as string | undefined) ?? `request failed with ${response.status}`,
    });
}

export const actions: Actions = {
    // Revives a started team: full health (game.health) and a fresh attack
    // clock for its current boss.
    revive: async ({ request, locals }) => {
        const teamId = String((await request.formData()).get('team_id') ?? '');
        return (await adminCall(locals.token, `/api/admin/teams/${encodeURIComponent(teamId)}/revive`, 'POST'))
            ?? { revived: teamId };
    },
    // Removes a member; new_leader is required (by the backend) when the
    // member leads the team. Errors come back as form data so the page can
    // show the backend's code and message.
    remove: async ({ request, locals }) => {
        if (!locals.token) redirect(303, '/');
        const form = await request.formData();
        const teamId = String(form.get('team_id') ?? '');
        const userId = String(form.get('user_id') ?? '');
        const newLeader = String(form.get('new_leader') ?? '');
        const query = newLeader ? `?new_leader=${encodeURIComponent(newLeader)}` : '';

        const response = await backendFetch(
            `/api/admin/teams/${encodeURIComponent(teamId)}/members/${encodeURIComponent(userId)}${query}`,
            locals.token, { method: 'DELETE' });
        if (response.ok) return { removed: { teamId, userId } };

        const data = await response.json().catch(() => ({}));
        return fail(response.status, {
            code: (data.code as string | undefined) ?? 'error',
            message: (data.message as string | undefined) ?? `request failed with ${response.status}`,
        });
    },
};
