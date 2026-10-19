import { z } from 'zod';

// The stream's event format (backend/internal/server/README.md, "Event
// format"): every event's data is {type, message}. type says why it was sent,
// message is the whole resource.
export const envelope = <T extends z.ZodType>(message: T) => z.object({ type: z.string(), message });

// version on team and team_state: orders copies of one resource that arrive
// from the stream and from write responses, in either order.
export const VersionSchema = z.object({ epoch: z.number(), seq: z.number() });
export type Version = z.infer<typeof VersionSchema>;

/**
 * Whether a copy at version incoming should replace the one held: always if
 * none is held or the epoch differs (the backend restarted), otherwise only
 * if its seq is higher. An equal version is the same state.
 */
export const isNewer = (held: Version | undefined, incoming: Version): boolean =>
    !held || held.epoch !== incoming.epoch || incoming.seq > held.seq;
