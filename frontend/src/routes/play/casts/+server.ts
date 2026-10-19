import { proxyJSON } from "#lib/server/backend.ts";
import type { RequestHandler } from "./$types";

// GET: the team's cast log, newest first, forwarded with the session token.
// /play re-fetches it when team_state.cast_count says it missed casts.
export const GET: RequestHandler = async ({ locals }) =>
    proxyJSON('/api/teams/me/casts', locals.token, 'GET');
