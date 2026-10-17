<script lang="ts">
    import { BACKEND_URL } from "$app/env/public";
    import type { PageProps } from "./$types";

    let { data }: PageProps = $props();
    let { user } = $derived(data);

    // A full-page navigation, not fetch: the backend redirects to Google.
    const loginUrl = `${BACKEND_URL.replace(/\/+$/, "")}/auth/google/login?next=${encodeURIComponent("/")}`;
</script>

{#if user}
    {JSON.stringify(user, null, 2)}

    <p><a href="/team">Team</a></p>

    <form method="POST" action="/auth/logout">
        <button type="submit"> Logout</button>
    </form>
{:else}
    <a href={loginUrl}> Login </a>
{/if}
