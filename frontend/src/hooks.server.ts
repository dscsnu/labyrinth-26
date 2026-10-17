import { sequence, type Handle } from "@sveltejs/kit/hooks";

import authenticate from "#lib/server/middleware/authenticate.ts";
import handleDevice from "#lib/server/middleware/handleDevice.ts";

export const handle: Handle = sequence(authenticate, handleDevice)