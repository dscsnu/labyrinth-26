import { z } from 'zod';

export const UserRoleSchema = z.enum(['PLAYER', 'HELPER', 'ADMIN']);

export const UserSchema = z.object({
    id: z.string(),
    email: z.string(),
    name: z.string(),
    avatar_url: z.string().nullable(),
    role: UserRoleSchema,
    // A helper's assigned checkpoint, whose QR their /qr shows; null if none.
    waypoint_id: z.string().nullable(),
    created_at: z.string(),
});

export type UserRole = z.infer<typeof UserRoleSchema>;
export type User = z.infer<typeof UserSchema>;

// What the backend JWT carries; available on every request without a backend call.
export type SessionUser = Pick<User, 'id' | 'email' | 'name' | 'avatar_url' | 'role'>;
