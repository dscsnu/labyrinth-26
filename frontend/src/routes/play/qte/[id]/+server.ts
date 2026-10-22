import { proxyJSON } from "#lib/server/backend.ts";
import type { RequestHandler } from "./$types";

// POST {"option"}: answer the open quiz for the team, once. Returns the team state.
export const POST: RequestHandler = async ({ locals, params, request }) =>
    proxyJSON(`/api/teams/me/state/qte/${encodeURIComponent(params.id)}/answer`, locals.token, 'POST', await request.json().catch(() => ({})));
