<script lang="ts">
    import { envelope, isNewer } from "#lib/types/Live.type.ts";
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

    // The backend's teams.size: a team holds up to 6, and the leader can
    // start once every other member is ready (backend/internal/teams/README.md).
    const PARTY_SIZE = 6;
    let others = $derived(team?.members.filter((m) => m.id !== team?.leader_id) ?? []);
    let readyCount = $derived(others.filter((m) => m.is_ready).length);
    let allReady = $derived(readyCount === others.length);
    let emptySlots = $derived(Math.max(0, PARTY_SIZE - (team?.members.length ?? 0)));

    const STATUS_LABEL: Record<Team["status"], string> = { LOBBY: "Lobby", IN_GAME: "In game", FINISHED: "Finished" };

    // Each member's avatar is a coloured block with their initial, the colour
    // picked from their id so it's the same on every phone.
    const AVATAR_COLOURS = ["bg-[#c0453a]", "bg-[#3a7bc0]", "bg-[#5fb04a]", "bg-[#9b59b6]", "bg-[#d98c2b]", "bg-[#2bb5a3]"];
    const avatarColour = (id: string) =>
        AVATAR_COLOURS[[...id].reduce((sum, c) => sum + c.charCodeAt(0), 0) % AVATAR_COLOURS.length];

    // Shared button looks: a pixel box that presses in.
    const button =
        "pixel-box block w-full px-4 py-3 text-[16px] uppercase active:translate-y-[2px] active:pixel-box-pressed disabled:pointer-events-none disabled:opacity-50";

    onMount(() => {
        // EventSource reconnects by itself; the first event after each
        // (re)connect is the full current state.
        const events = new EventSource("/api/events");
        events.onopen = () => (connected = true);
        events.onerror = () => (connected = false);
        // Events are {type, message}. Keep a team if it's another team than
        // the one shown, or a newer copy of it (it may race the page load).
        events.addEventListener("team", (e) => {
            const next = envelope(TeamSchema).parse(JSON.parse(e.data)).message;
            if (next.id !== team?.id || isNewer(team.version, next.version)) live = next;
        });
        events.addEventListener("no_team", () => {
            live = null;
        });
        return () => events.close();
    });
</script>

<svelte:head><title>Party · Labyrinth</title></svelte:head>

<div
    class="min-h-dvh bg-night bg-[repeating-conic-gradient(var(--color-night)_0_25%,#30264a_0_50%)] bg-[length:16px_16px] pb-12 font-pixel text-[16px] leading-[24px] text-parchment"
>
    <!-- Top bar, the same gold as /play's -->
    <header class="flex items-center justify-between bg-gold px-4 py-3 text-ink shadow-[0_4px_0_0_var(--color-gold-dark),0_8px_0_0_var(--color-ink)]">
        <a href="/" class="uppercase hover:text-parchment">&lt; Home</a>
        <span class="flex items-center gap-2 text-[8px] uppercase">
            {#if connected}
                <span class="h-2 w-2 bg-go motion-safe:animate-blink"></span> Live
            {:else}
                <span class="h-2 w-2 bg-danger"></span> Reconnecting
            {/if}
        </span>
    </header>

    <main class="mx-auto flex max-w-md flex-col gap-8 px-5 pt-10">
        <div>
            <h1 class="text-[32px] leading-[40px] text-gold-light uppercase [text-shadow:4px_4px_0_var(--color-ink)]">Party</h1>
            <p class="mt-2 text-[8px] uppercase opacity-80">Player: {user?.name}</p>
        </div>

        {#if form?.code}
            <div role="alert" class="pixel-box bg-danger p-4">
                <p class="uppercase">! Error</p>
                <p class="mt-2 text-[8px] leading-[16px] uppercase [overflow-wrap:anywhere]">
                    {form.action}: {form.code}: {form.message}
                </p>
            </div>
        {/if}

        {#if !team}
            <!-- No team: create one or join one by its code -->
            <form method="POST" action="?/create" class="pixel-box flex flex-col gap-4 bg-panel p-5">
                <h2 class="text-gold-light uppercase">New party</h2>
                <label class="flex flex-col gap-2">
                    <span class="text-[8px] uppercase">Party name</span>
                    <input
                        name="name"
                        maxlength="32"
                        required
                        autocomplete="off"
                        placeholder="Seven Exes"
                        class="pixel-box bg-ink px-3 py-3 text-parchment placeholder:text-parchment/30 focus:outline-4 focus:outline-offset-4 focus:outline-gold-light"
                    />
                </label>
                <button class="{button} bg-gold text-ink hover:bg-gold-light">Create</button>
            </form>

            <p class="text-center text-[8px] uppercase opacity-80">- or -</p>

            <form method="POST" action="?/join" class="pixel-box flex flex-col gap-4 bg-panel p-5">
                <h2 class="text-gold-light uppercase">Join a party</h2>
                <label class="flex flex-col gap-2">
                    <span class="text-[8px] uppercase">Party code</span>
                    <input
                        name="team_id"
                        inputmode="numeric"
                        pattern="[0-9]{6}"
                        maxlength="6"
                        required
                        autocomplete="off"
                        placeholder="000000"
                        class="pixel-box bg-ink px-3 py-3 text-center text-[24px] leading-[32px] tracking-[8px] text-parchment placeholder:text-parchment/30 focus:outline-4 focus:outline-offset-4 focus:outline-gold-light"
                    />
                </label>
                <button class="{button} bg-gold text-ink hover:bg-gold-light">Join</button>
            </form>
        {:else}
            <!-- The team: name, status and the code to share -->
            <section class="pixel-box flex flex-col gap-4 bg-panel p-5">
                <div class="flex items-start justify-between gap-4">
                    <h2 class="text-[24px] leading-[32px] text-gold-light [overflow-wrap:anywhere]">{team.name}</h2>
                    <span
                        class="shrink-0 px-2 py-1 text-[8px] uppercase {team.status === 'IN_GAME' ? 'bg-go text-ink' : team.status === 'LOBBY' ? 'bg-gold text-ink' : 'bg-ink text-parchment'}"
                    >
                        {STATUS_LABEL[team.status]}
                    </span>
                </div>
                <div class="pixel-box bg-ink px-4 py-3 text-center">
                    <p class="text-[8px] uppercase opacity-80">Party code</p>
                    <p class="mt-2 text-[32px] leading-[40px] tracking-[8px] text-gold-light">{team.id}</p>
                    {#if team.status === "LOBBY"}
                        <p class="mt-2 text-[8px] uppercase opacity-80">Share it to recruit</p>
                    {/if}
                </div>
            </section>

            {#if team.status === "IN_GAME"}
                <a href="/play" class="{button} bg-go text-center text-ink hover:brightness-110">
                    Enter the labyrinth <span class="motion-safe:animate-blink">&gt;</span>
                </a>
            {/if}

            <!-- The roster: one slot per member, then the empty ones -->
            <section class="pixel-box flex flex-col gap-3 bg-panel p-5">
                <div class="flex items-baseline justify-between">
                    <h2 class="text-gold-light uppercase">Members</h2>
                    <span class="text-[8px] uppercase">{team.members.length}/{PARTY_SIZE}</span>
                </div>

                <ul class="flex flex-col gap-3">
                    {#each team.members as m (m.id)}
                        {@const leader = m.id === team.leader_id}
                        <li class="flex items-center gap-3 bg-ink/60 p-2 {m.id === user?.id ? 'outline-2 outline-gold' : ''}">
                            <span class="flex h-10 w-10 shrink-0 items-center justify-center text-ink {avatarColour(m.id)}">
                                {m.name.charAt(0).toUpperCase()}
                            </span>
                            <span class="flex min-w-0 flex-1 flex-col gap-1">
                                <span class="truncate">{m.name}</span>
                                <span class="flex flex-wrap gap-2 text-[8px] leading-[16px] uppercase">
                                    {#if leader}
                                        <span class="bg-gold px-1 text-ink">Leader</span>
                                    {:else if m.is_ready}
                                        <span class="bg-go px-1 text-ink">Ready</span>
                                    {:else}
                                        <span class="bg-danger px-1">Not ready</span>
                                    {/if}
                                    {#if m.id === user?.id}<span class="text-gold-light">You</span>{/if}
                                </span>
                            </span>
                        </li>
                    {/each}
                    {#each { length: emptySlots }, i (i)}
                        <li class="flex items-center gap-3 border-2 border-dashed border-parchment/20 p-2 text-parchment/40">
                            <span class="h-10 w-10 shrink-0 bg-ink/40"></span>
                            <span class="text-[8px] uppercase">Empty slot</span>
                        </li>
                    {/each}
                </ul>
            </section>

            {#if team.status === "LOBBY"}
                <div class="flex flex-col gap-6">
                    {#if isLeader}
                        <form method="POST" action="?/start" class="flex flex-col gap-3">
                            <button disabled={!allReady} class="{button} bg-gold text-ink hover:bg-gold-light">Start game</button>
                            <p class="text-center text-[8px] uppercase opacity-80">
                                {#if others.length === 0}
                                    Solo run: start whenever
                                {:else if allReady}
                                    Everyone's ready
                                {:else}
                                    Waiting: {readyCount}/{others.length} ready
                                {/if}
                            </p>
                        </form>
                    {:else}
                        <form method="POST" action="?/ready">
                            <input type="hidden" name="ready" value={me?.is_ready ? "false" : "true"} />
                            <button class="{button} {me?.is_ready ? 'bg-panel text-parchment' : 'bg-go text-ink hover:brightness-110'}">
                                {me?.is_ready ? "Unready" : "Ready up"}
                            </button>
                        </form>
                    {/if}
                    <form method="POST" action="?/leave">
                        <button class="{button} bg-danger text-parchment hover:brightness-110">Leave party</button>
                    </form>
                </div>
            {/if}
        {/if}
    </main>
</div>
