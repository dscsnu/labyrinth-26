import { apiResponse, backendFetch } from "#lib/server/backend.ts";
import { TeamSchema } from "#lib/types/Team.type.ts";
import { fail, redirect } from "@sveltejs/kit";
import type { Actions, PageServerLoad } from "./$types";

const TeamResponseSchema = apiResponse(TeamSchema);

export const load: PageServerLoad = async ({ locals }) => {
    if (!locals.user || !locals.token) redirect(303, '/');

    const response = await backendFetch('/api/teams/me', locals.token);
    if (response.status === 404) return { team: null };
    if (!response.ok) throw new Error(`GET /api/teams/me failed with ${response.status}`);

    return { team: TeamResponseSchema.parse(await response.json()).payload ?? null };
}

// Calls the backend on the user's behalf. Errors come back as form data so
// the page can show the backend's code and message.
const call = async (token: string | null, action: string, method: string, path: string, body?: unknown) => {
    if (!token) redirect(303, '/');

    const response = await backendFetch(path, token, {
        method,
        headers: { 'Content-Type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (response.ok) return;

    const data = await response.json().catch(() => ({}));
    return fail(response.status, {
        action,
        code: (data.code as string | undefined) ?? 'error',
        message: (data.message as string | undefined) ?? `request failed with ${response.status}`,
    });
}

export const actions: Actions = {
    create: async ({ request, locals }) => {
        const form = await request.formData();
        return call(locals.token, 'create', 'POST', '/api/teams', { name: String(form.get('name') ?? '') });
    },
    join: async ({ request, locals }) => {
        const form = await request.formData();
        return call(locals.token, 'join', 'POST', '/api/teams/join', { team_id: String(form.get('team_id') ?? '') });
    },
    ready: async ({ request, locals }) => {
        const form = await request.formData();
        return call(locals.token, 'ready', 'PUT', '/api/teams/me/ready', { ready: form.get('ready') === 'true' });
    },
    start: async ({ locals }) => call(locals.token, 'start', 'POST', '/api/teams/me/start'),
    leave: async ({ locals }) => call(locals.token, 'leave', 'POST', '/api/teams/leave'),
};