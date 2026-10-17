import { dev } from "$app/env";
import { apiResponse, backendFetch, TOKEN_COOKIE } from "#lib/server/backend.ts";
import { UserSchema } from "#lib/types/User.type.ts";
import safeNext from "#lib/utils/safeNext.ts";
import { redirect } from "@sveltejs/kit";
import { z } from "zod";
import type { RequestHandler } from "./$types";

const TokenResponseSchema = apiResponse(z.object({
    access_token: z.string(),
    token_type: z.literal('Bearer'),
    expires_at: z.string(),
    user: UserSchema,
}));

type TokenPayload = NonNullable<z.infer<typeof TokenResponseSchema>['payload']>;

const exchangeCode = async (code: string): Promise<TokenPayload | null> => {
    try {
        const response = await backendFetch('/auth/token', null, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ code }),
        });
        if (!response.ok) {
            console.error(`POST /auth/token failed with ${response.status}`);
            return null;
        }
        return TokenResponseSchema.parse(await response.json()).payload ?? null;
    } catch (e) {
        console.error(`POST /auth/token failed > ${e}`);
        return null;
    }
}

// The backend redirects here after Google sign-in with a one-time code, which
// is exchanged server-to-server for the backend JWT.
export const GET: RequestHandler = async ({ url, cookies }) => {
    const code = url.searchParams.get('code');
    if (!code) redirect(303, '/auth/error?reason=code_missing');

    const payload = await exchangeCode(code);
    if (!payload) redirect(303, '/auth/error?reason=token_exchange_failed');

    cookies.set(TOKEN_COOKIE, payload.access_token, {
        path: '/',
        httpOnly: true,
        secure: !dev,
        sameSite: 'lax',
        expires: new Date(payload.expires_at),
    });

    redirect(303, safeNext(url.searchParams.get('next')));
}