import { proxyJSON } from "#lib/server/backend.ts";
import type { RequestHandler } from "./$types";

// POST {"qr"}: the browser's scan (the QR's signed token), forwarded with the session token.
export const POST: RequestHandler = async ({ locals, request }) =>
    proxyJSON('/api/teams/me/state/scans', locals.token, 'POST', await request.json().catch(() => ({})));
