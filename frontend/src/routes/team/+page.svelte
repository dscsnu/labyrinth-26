<script lang="ts">
    import { TeamSchema, type Team } from "#lib/types/Team.type.ts";
    import { onMount } from "svelte";
    import type { PageProps } from "./$types";

    let { data, form }: PageProps = $props();
    let { user } = $derived(data);

    // Latest state from the event stream; until the first event, the page load's.
    let live = $state<Team | null | undefined>(undefined);
    let connected = $state(false);
    let team = $derived(live === undefined ? data.team : live);

    let isLeader = $derived(team?.leader_id === user?.id);
    let me = $derived(team?.members.find((m) => m.id === user?.id));

    onMount(() => {
        // EventSource reconnects by itself; the first event after each
        // (re)connect is the full current state.
        const events = new EventSource("/api/events");
        events.onopen = () => (connected = true);
        events.onerror = () => (connected = false);
        events.addEventListener("team", (e) => {
            live = TeamSchema.parse(JSON.parse(e.data));
        });
        events.addEventListener("no_team", () => {
            live = null;
        });
        return () => events.close();
    });
</script>

<p>
    <a href="/">home</a> · signed in as {user?.name} ({user?.role}) ·
    {connected ? "live ●" : "reconnecting…"}
</p>

{#if form?.code}
    <p style="color: red">[{form.action}] {form.code}: {form.message}</p>
{/if}

{#if !team}
    <h1>No team</h1>

    <form method="POST" action="?/create">
        <label>Team name <input name="name" maxlength="32" required /></label>
        <button>Create team</button>
    </form>

    <form method="POST" action="?/join">
        <label>Team code <input name="team_id" inputmode="numeric" maxlength="6" required /></label>
        <button>Join team</button>
    </form>
{:else}
    <h1>{team.name}</h1>
    {#if team.status === "IN_GAME"}<p><a href="/play"><b>→ Play</b></a></p>{/if}
    <p>code: <b>{team.id}</b> · status: <b>{team.status}</b> · {team.members.length}/6</p>

    <ul>
        {#each team.members as m (m.id)}
            <li>
                {m.name}
                {#if m.id === team.leader_id}(leader){:else}— {m.is_ready ? "ready" : "not ready"}{/if}
                {#if m.id === user?.id}<b>← you</b>{/if}
            </li>
        {/each}
    </ul>

    {#if team.status === "LOBBY"}
        {#if isLeader}
            <form method="POST" action="?/start"><button>Start game</button></form>
        {:else}
            <form method="POST" action="?/ready">
                <input type="hidden" name="ready" value={me?.is_ready ? "false" : "true"} />
                <button>{me?.is_ready ? "Unready" : "Ready"}</button>
            </form>
        {/if}
        <form method="POST" action="?/leave"><button>Leave team</button></form>
    {/if}
{/if}