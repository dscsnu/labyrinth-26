import { proxyJSON } from "#lib/server/backend.ts";
import type { RequestHandler } from "./$types";

// GET: the signing key for the checkpoint an admin assigned the caller,
// forwarded with the session token (404 not_assigned until there is one).
export const GET: RequestHandler = async ({ locals }) =>
    proxyJSON('/api/qr/me', locals.token, 'GET');
