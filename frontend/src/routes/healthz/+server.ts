import type { RequestHandler } from "./$types";

// GET: 200 while the server is up, for the container's healthcheck
// (compose.prod.yaml). Touches nothing else: the backend has its own.
export const GET: RequestHandler = () =>
    new Response('ok', { headers: { 'Cache-Control': 'no-store' } });
