import { error } from "@sveltejs/kit";
import { dev } from "$app/env";
import { TEST_PAGES } from "$app/env/private";
import type { PageServerLoad } from "./$types";

// A tool for checking the bosses' animations: open under `bun run dev`,
// and in a build only with TEST_PAGES=true, never for players.
export const load: PageServerLoad = () => {
    if (!dev && !TEST_PAGES) error(404, "Not found");
};
