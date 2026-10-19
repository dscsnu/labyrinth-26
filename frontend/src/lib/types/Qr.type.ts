import { z } from 'zod';

// GET /api/qr/{waypoint_id}: the waypoint-scoped HMAC key and timing rules the
// /qr display uses to sign its own rotating tokens.
export const SigningKeySchema = z.object({
    waypoint_id: z.string(),
    key: z.string(),
    rotate_ms: z.number(),
    window_ms: z.number(),
    server_time: z.number(),
});

export type SigningKey = z.infer<typeof SigningKeySchema>;

// POST /api/qr/verify: the waypoint a verified token stands for.
export const QRVerifySchema = z.object({
    waypoint_id: z.string(),
});

export type QRVerify = z.infer<typeof QRVerifySchema>;
