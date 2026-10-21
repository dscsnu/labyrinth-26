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
    <p><a href="/test-scan">Test scan</a></p>
    <p><a href="/leaderboard">Leaderboard</a></p>
    {#if user.role === "ADMIN"}<p><a href="/admin">Admin</a></p>{/if}
    {#if user.role === "HELPER" || user.role === "ADMIN"}<p><a href="/qr">QR display</a></p>{/if}

    <form method="POST" action="/auth/logout">
        <button type="submit"> Logout</button>
    </form>
{:else}
    <a href={loginUrl}> Login </a>
{/if}
