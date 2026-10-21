import { TOKEN_COOKIE } from "#lib/server/backend.ts";
import { verifySession } from "#lib/server/session.ts";
import type { Handle } from "@sveltejs/kit/hooks";

// Resolves locals.user from the backend JWT cookie, verified locally, with no
// backend call. Role is as of sign-in; a changed role applies on next sign-in.
const authenticate: Handle = async ({ event, resolve }) => {
    event.locals.user = null;
    event.locals.token = null;

    const token = event.cookies.get(TOKEN_COOKIE);
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
