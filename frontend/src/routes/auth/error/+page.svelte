<script lang="ts">
    import { page } from "$app/state";

    const messages: Record<string, string> = {
        google_access_denied: "Sign-in was cancelled.",
        email_unverified: "Your Google account's email isn't verified.",
        email_domain_not_allowed: "Please sign in with your university Google account.",
    };

    let reason = $derived(page.url.searchParams.get("reason") ?? "unknown");
    let message = $derived(messages[reason] ?? "Something went wrong while signing in.");
</script>

<p>{message}</p>
<p>({reason})</p>

<a href="/">Back</a>
