import { proxyJSON } from "#lib/server/backend.ts";
import type { RequestHandler } from "./$types";

// POST: the leader uses a held powerup; instant_cast takes {"pattern": [IDs]}.
// Returns the team state.
export const POST: RequestHandler = async ({ locals, params, request }) =>
    proxyJSON(`/api/teams/me/state/powerups/${encodeURIComponent(params.kind)}`, locals.token, 'POST', await request.json().catch(() => ({})));
