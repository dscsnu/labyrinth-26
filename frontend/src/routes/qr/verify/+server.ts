import { proxyJSON } from "#lib/server/backend.ts";
import type { RequestHandler } from "./$types";

// POST {"qr"}: verify a scanned token, forwarded with the session token.
export const POST: RequestHandler = async ({ locals, request }) =>
    proxyJSON('/api/qr/verify', locals.token, 'POST', await request.json().catch(() => ({})));
