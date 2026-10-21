import { dev } from "$app/env";
import { DEV_TOKEN } from "$app/env/private";
import { TOKEN_COOKIE } from "#lib/server/backend.ts";
import { verifySession } from "#lib/server/session.ts";
import type { Handle } from "@sveltejs/kit/hooks";

// Resolves locals.user from the backend JWT cookie, verified locally, with no
// backend call. Role is as of sign-in; a changed role applies on next sign-in.
const authenticate: Handle = async ({ event, resolve }) => {
    event.locals.user = null;
    event.locals.token = null;

    // In dev, fall back to DEV_TOKEN so devices that can't sign in still get a session.
    const token = event.cookies.get(TOKEN_COOKIE) ?? (dev ? DEV_TOKEN : undefined);
    if (!token) return resolve(event);

    const user = await verifySession(token);
    if (user) {
        event.locals.user = user;
        event.locals.token = token;
    } else {
        // expired, tampered, or signed with a rotated key
        event.cookies.delete(TOKEN_COOKIE, { path: '/' });
    }

    return resolve(event);
}

export default authenticate;
