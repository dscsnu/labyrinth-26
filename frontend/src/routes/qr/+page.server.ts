import { redirect } from "@sveltejs/kit";
import type { PageServerLoad } from "./$types";

// TEMPORARY (dev): any signed-in user may open /qr. In production this checks
// the backend role (HELPER/ADMIN) via GET /api/me.
export const load: PageServerLoad = async ({ locals }) => {
    if (!locals.user || !locals.token) redirect(303, '/');
};
