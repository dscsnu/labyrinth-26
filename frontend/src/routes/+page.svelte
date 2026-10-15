<script lang="ts">
    import getCustomClaims from "#lib/utils/getCustomClaims.ts";
    import type { PageProps } from "./$types";

    let { data }: PageProps = $props();
    let { supabase, session, user } = $derived(data);

    const handleSignIn = async () => {
        await supabase.auth.signInWithOAuth({
            provider: "google",
            options: {
                redirectTo: `${window.location.origin}/api/auth/callback`,
                queryParams: {
                    access_type: "offline",
                    prompt: "consent",
                },
            },
        });
    };
</script>

<button onclick={handleSignIn}> Login </button>

{#if user}
    {JSON.stringify(getCustomClaims(user), null, 2)}

    <button onclick={() => supabase.auth.signOut()}> Logout</button>
{/if}
