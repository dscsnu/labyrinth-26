import { BACKEND_INTERNAL_URL } from "$app/env/private";
import { BACKEND_URL } from "$app/env/public";
import { z } from "zod";

// httpOnly cookie holding the backend JWT. Only the SvelteKit server reads it.
export const TOKEN_COOKIE = 'labyrinth_token';

// The server's calls go to BACKEND_INTERNAL_URL when set (production: the
// Docker network); the browser always uses the public BACKEND_URL.
export const backendUrl = (path: string): string =>
    `${(BACKEND_INTERNAL_URL ?? BACKEND_URL).replace(/\/+$/, '')}${path}`;

// Every backend response uses this envelope.
export const apiResponse = <T extends z.ZodType>(payload: T) => z.object({
    success: z.boolean(),
    message: z.string().optional(),
    payload: payload.optional(),
});

/**
 * Calls the backend from the SvelteKit server, attaching the user's JWT when given.
 */
export const backendFetch = (path: string, token: string | null, init: RequestInit = {}): Promise<Response> => {
    const headers = new Headers(init.headers);
    if (token) headers.set('Authorization', `Bearer ${token}`);
    return fetch(backendUrl(path), { ...init, headers });
}

/**
 * Forwards a JSON call to the backend for the browser and returns the
 * backend's status and envelope unchanged, so client code sees exactly what
 * the backend said (including error codes).
 */
export const proxyJSON = async (path: string, token: string | null, method: string, body?: unknown): Promise<Response> => {
    if (!token) return Response.json({ success: false, code: 'not_signed_in', message: 'not signed in' }, { status: 401 });
    const upstream = await backendFetch(path, token, {
        method,
        headers: { 'Content-Type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
    });
    return new Response(upstream.body, {
        status: upstream.status,
        headers: { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' },
    });
}
