import { backendFetch } from "#lib/server/backend.ts";
import { error } from "@sveltejs/kit";
import type { RequestHandler } from "./$types";

// Proxies the backend's team event stream so the browser can use a plain
// same-origin EventSource; the httpOnly session cookie authenticates it here.
// Aborting the browser request closes the backend stream too.
export const GET: RequestHandler = async ({ locals, request }) => {
    if (!locals.token) error(401, 'not signed in');

    const upstream = await backendFetch('/api/teams/me/events', locals.token, {
        signal: request.signal,
    });
    if (!upstream.ok || !upstream.body) error(upstream.status === 401 ? 401 : 502, 'event stream unavailable');

    return new Response(upstream.body, {
        headers: {
            'Content-Type': 'text/event-stream',
            'Cache-Control': 'no-store',
            'X-Accel-Buffering': 'no',
        },
    });
}