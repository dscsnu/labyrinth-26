import { z } from 'zod';

export const TeamMemberSchema = z.object({
    id: z.string(),
    name: z.string(),
    avatar_url: z.string().nullable(),
    is_ready: z.boolean(),
    joined_at: z.string(),
});

export const TeamSchema = z.object({
    id: z.string(),
    name: z.string(),
    leader_id: z.string(),
    status: z.enum(['LOBBY', 'IN_GAME', 'FINISHED']),
    created_at: z.string(),
    started_at: z.string().nullable(),
    members: z.array(TeamMemberSchema),
});

export type TeamMember = z.infer<typeof TeamMemberSchema>;
export type Team = z.infer<typeof TeamSchema>;