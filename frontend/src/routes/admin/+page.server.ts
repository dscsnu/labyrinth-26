import { apiResponse, backendFetch } from "#lib/server/backend.ts";
import { AdminQTEListSchema, TeamOverviewListSchema, UserListSchema } from "#lib/types/Admin.type.ts";
import { error, fail, redirect } from "@sveltejs/kit";
import type { Actions, PageServerLoad } from "./$types";

const TeamsResponseSchema = apiResponse(TeamOverviewListSchema);
const UsersResponseSchema = apiResponse(UserListSchema);
const QTEsResponseSchema = apiResponse(AdminQTEListSchema);
const HelpersResponseSchema = apiResponse(UserListSchema);

// The backend decides who's an admin, from the role in the database: the
// session's role is as of sign-in and may be stale, so it isn't trusted here.
// ?email= searches users for the role tools.
export const load: PageServerLoad = async ({ locals, url }) => {
    if (!locals.user || !locals.token) redirect(303, '/');

    const email = url.searchParams.get('email');
    const [teamsRes, qtesRes, helpersRes, usersRes] = await Promise.all([
        backendFetch('/api/admin/teams', locals.token),
        backendFetch('/api/admin/qtes', locals.token),
        backendFetch('/api/admin/helpers', locals.token),
        email === null ? null : backendFetch(`/api/admin/users?email=${encodeURIComponent(email)}`, locals.token),
    ]);
    if (teamsRes.status === 403) error(403, 'Admins only');
    if (!teamsRes.ok) throw new Error(`GET /api/admin/teams failed with ${teamsRes.status}`);
    if (!qtesRes.ok) throw new Error(`GET /api/admin/qtes failed with ${qtesRes.status}`);
    if (!helpersRes.ok) throw new Error(`GET /api/admin/helpers failed with ${helpersRes.status}`);
    if (usersRes && !usersRes.ok) throw new Error(`GET /api/admin/users failed with ${usersRes.status}`);

    return {
        teams: TeamsResponseSchema.parse(await teamsRes.json()).payload ?? [],
        qtes: QTEsResponseSchema.parse(await qtesRes.json()).payload ?? [],
        helpers: HelpersResponseSchema.parse(await helpersRes.json()).payload ?? [],
        email,
        users: usersRes ? UsersResponseSchema.parse(await usersRes.json()).payload ?? [] : null,
    };
}

// Calls an admin route; errors come back as form data with the backend's
// code and message. section names where the page shows the result (the
// helpers' forms show theirs beside them); unset, at the top.
const adminCall = async (token: string | null, path: string, method: string, body?: unknown, section?: 'helpers') => {
    if (!token) redirect(303, '/');
    const response = await backendFetch(path, token, {
        method,
        headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (response.ok) return null;
    const data = await response.json().catch(() => ({}));
    return fail(response.status, {
        code: (data.code as string | undefined) ?? 'error',
        message: (data.message as string | undefined) ?? `request failed with ${response.status}`,
        section,
    });
}

// field reads one form field as a string.
const field = (form: FormData, name: string) => String(form.get(name) ?? '');

// Creates a quiz from a form's question, options (one per line), correct
// (the right option's number, from 1), powerup and window_s.
const createQuiz = (token: string | null, form: FormData, start: boolean) => {
    const window = field(form, 'window_s');
    return adminCall(token, '/api/admin/qtes', 'POST', {
        kind: 'quiz',
        powerup: field(form, 'powerup'),
        quiz: {
            question: field(form, 'question').trim(),
            options: field(form, 'options').split('\n').map((o) => o.trim()).filter(Boolean),
            correct_option: Number(field(form, 'correct')) - 1,
        },
        window_s: window ? Number(window) : undefined,
        start,
    });
}

export const actions: Actions = {
    // Health back to game.health and the boss's attack clock restarted.
    revive: async ({ request, locals }) => {
        const teamId = field(await request.formData(), 'team_id');
        return (await adminCall(locals.token, `/api/admin/teams/${encodeURIComponent(teamId)}/revive`, 'POST'))
            ?? { done: `Revived team ${teamId}: full health, and its boss's attack clock restarted.` };
    },
    // No spell class or waypoint on cooldown for the team any more.
    reset: async ({ request, locals }) => {
        const teamId = field(await request.formData(), 'team_id');
        return (await adminCall(locals.token, `/api/admin/teams/${encodeURIComponent(teamId)}/cooldowns/reset`, 'POST'))
            ?? { done: `Reset team ${teamId}'s cooldowns.` };
    },
    // The team and all its progress go; its members become teamless.
    delete: async ({ request, locals }) => {
        const teamId = field(await request.formData(), 'team_id');
        return (await adminCall(locals.token, `/api/admin/teams/${encodeURIComponent(teamId)}`, 'DELETE'))
            ?? { done: `Deleted team ${teamId}. Its members can create or join a team.` };
    },
    // Closes the open quiz now, revealing the answer.
    endQte: async ({ request, locals }) => {
        const id = field(await request.formData(), 'qte_id');
        return (await adminCall(locals.token, `/api/admin/qtes/${encodeURIComponent(id)}/end`, 'POST'))
            ?? { done: `Quiz ${id} ended.` };
    },
    // A new quiz: options one per line, the right one by its number (from
    // 1), saved as a draft or started at once. The backend checks it.
    createQte: async ({ request, locals }) => {
        const form = await request.formData();
        const start = form.get('start') === 'on';
        return (await createQuiz(locals.token, form, start)) ?? { done: start ? 'Quiz started.' : 'Quiz saved as a draft.' };
    },
    // Sends a listed quiz out for the prize picked: a draft keeping its
    // prize is started as it is; otherwise (a closed one, or a new prize)
    // a copy is, since a quiz runs once and its prize can't be edited.
    sendQte: async ({ request, locals }) => {
        const form = await request.formData();
        const id = field(form, 'qte_id');
        if (field(form, 'status') === 'draft' && field(form, 'powerup') === field(form, 'original_powerup')) {
            return (await adminCall(locals.token, `/api/admin/qtes/${encodeURIComponent(id)}/start`, 'POST'))
                ?? { done: `Quiz ${id} started.` };
        }
        return (await createQuiz(locals.token, form, true))
            ?? { done: `Quiz ${id} sent as a new quiz, for ${field(form, 'powerup')}.` };
    },
    // Removes a member; new_leader is required (by the backend) when the
    // member leads the team.
    remove: async ({ request, locals }) => {
        const form = await request.formData();
        const teamId = field(form, 'team_id');
        const userId = field(form, 'user_id');
        const newLeader = field(form, 'new_leader');
        const query = newLeader ? `?new_leader=${encodeURIComponent(newLeader)}` : '';
        return (await adminCall(locals.token,
            `/api/admin/teams/${encodeURIComponent(teamId)}/members/${encodeURIComponent(userId)}${query}`, 'DELETE'))
            ?? { done: "Removed. They're teamless and can create or join a team." };
    },
    // Gives a user PLAYER, HELPER or ADMIN (not your own).
    role: async ({ request, locals }) => {
        const form = await request.formData();
        const userId = field(form, 'user_id');
        const role = field(form, 'role');
        return (await adminCall(locals.token, `/api/admin/users/${encodeURIComponent(userId)}/role`, 'PUT', { role }))
            ?? { done: `Role set to ${role}.` };
    },
    // Makes the account with this email a helper; they must have signed in
    // once. The backend checks it exists.
    addHelper: async ({ request, locals }) => {
        const email = field(await request.formData(), 'email').trim();
        return (await adminCall(locals.token, '/api/admin/helpers', 'POST', { email }, 'helpers'))
            ?? { done: `${email} is a helper. Assign their checkpoint below.`, section: 'helpers' };
    },
    // Sets the checkpoint a helper's /qr shows; empty unassigns.
    assignWaypoint: async ({ request, locals }) => {
        const form = await request.formData();
        const userId = field(form, 'user_id');
        const waypoint = field(form, 'waypoint_id') || null;
        return (await adminCall(locals.token, `/api/admin/helpers/${encodeURIComponent(userId)}/waypoint`, 'PUT', { waypoint_id: waypoint }, 'helpers'))
            ?? { done: waypoint ? `Checkpoint ${waypoint} assigned.` : 'Checkpoint unassigned.', section: 'helpers' };
    },
    // No longer a helper: back to PLAYER, which also clears the checkpoint.
    removeHelper: async ({ request, locals }) => {
        const userId = field(await request.formData(), 'user_id');
        return (await adminCall(locals.token, `/api/admin/users/${encodeURIComponent(userId)}/role`, 'PUT', { role: 'PLAYER' }, 'helpers'))
            ?? { done: 'Helper removed.', section: 'helpers' };
    },
};
