import { TOKEN_COOKIE } from "#lib/server/backend.ts";
import { redirect } from "@sveltejs/kit";
import type { RequestHandler } from "./$types";

// Tokens are stateless, so signing out just drops the cookie.
export const POST: RequestHandler = async ({ cookies }) => {
    cookies.delete(TOKEN_COOKIE, { path: '/' });
    redirect(303, '/');
}