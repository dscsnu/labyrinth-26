import { z } from 'zod';

export const CustomClaimsSchema = z.object({
    role: z.enum(['PLAYER', 'HELPER', 'ADMIN']),
});

export type CustomClaims = z.infer<typeof CustomClaimsSchema>;