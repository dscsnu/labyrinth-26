import { JWT_PUBLIC_KEY } from "$app/env/private";
import { UserRoleSchema, type SessionUser } from "#lib/types/User.type.ts";
import { importSPKI, jwtVerify, type CryptoKey } from "jose";
import { z } from "zod";

// Must match the backend's issuer/audience in backend/internal/auth/read.go.
const ISSUER = 'labyrinth-backend';
const AUDIENCE = 'labyrinth-api';

const SessionClaimsSchema = z.object({
    sub: z.string(),
    email: z.string(),
    name: z.string(),
    avatar_url: z.string().nullable(),
    role: UserRoleSchema,
});

let publicKey: Promise<CryptoKey> | null = null;
const getPublicKey = (): Promise<CryptoKey> =>
    publicKey ??= importSPKI(
        `-----BEGIN PUBLIC KEY-----\n${JWT_PUBLIC_KEY.trim()}\n-----END PUBLIC KEY-----`,
        'EdDSA',
    );

/**
 * Verifies a backend JWT locally with the public key. Returns null for any
 * invalid, expired, or tampered token. A misconfigured JWT_PUBLIC_KEY throws
 * instead, so it fails loudly rather than signing everyone out.
 */
export const verifySession = async (token: string): Promise<SessionUser | null> => {
    const key = await getPublicKey();

    try {
        const { payload } = await jwtVerify(token, key, {
            issuer: ISSUER,
            audience: AUDIENCE,
            algorithms: ['EdDSA'],
            requiredClaims: ['sub', 'exp'],
        });
        const claims = SessionClaimsSchema.parse(payload);
        return {
            id: claims.sub,
            email: claims.email,
            name: claims.name,
            avatar_url: claims.avatar_url,
            role: claims.role,
        };
    } catch {
        return null;
    }
}
