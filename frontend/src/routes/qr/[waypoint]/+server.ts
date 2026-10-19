import { proxyJSON } from "#lib/server/backend.ts";
import type { RequestHandler } from "./$types";

// GET: a fresh rotating token for the waypoint, forwarded with the session
// token. The backend enforces the HELPER/ADMIN role.
export const GET: RequestHandler = async ({ locals, params }) =>
    proxyJSON(`/api/qr/${encodeURIComponent(params.waypoint)}`, locals.token, 'GET');
