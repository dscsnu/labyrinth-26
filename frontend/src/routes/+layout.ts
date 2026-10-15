import { SUPABASE_PUBLISHABLE_KEY, SUPABASE_URL } from "$app/env/public";
import { createBrowserClient, createServerClient, isBrowser } from "@supabase/ssr";
import type { LayoutLoad } from "./$types";

export const load: LayoutLoad = async ({ data, depends, fetch }) => {
    depends('supabase:auth');

    const supabase = isBrowser()
        ? createBrowserClient(SUPABASE_URL, SUPABASE_PUBLISHABLE_KEY, {
            global: {
                fetch,
            },
        })
        : createServerClient(SUPABASE_URL, SUPABASE_PUBLISHABLE_KEY, {
            global: {
                fetch,
            },
            cookies: {
                getAll() {
                    return data.cookies
                },
            },
        });

    return {
        supabase,
        session: data.session,
        user: data.user,
    }
}