import { proxyJSON } from "#lib/server/backend.ts";
import type { RequestHandler } from "./$types";

// PUT {"shape"}: select a pattern type (purges the team's stack).
export const PUT: RequestHandler = async ({ locals, request }) =>
    proxyJSON('/api/teams/me/state/selection', locals.token, 'PUT', await request.json().catch(() => ({})));

// DELETE: give up the selection.
export const DELETE: RequestHandler = async ({ locals }) =>
    proxyJSON('/api/teams/me/state/selection', locals.token, 'DELETE');
