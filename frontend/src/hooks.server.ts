import { sequence, type Handle } from "@sveltejs/kit/hooks";

import createSupabase from "#lib/server/middleware/createSupabase.ts";
import handleDevice from "#lib/server/middleware/handleDevice.ts";

export const handle: Handle = sequence(createSupabase, handleDevice)