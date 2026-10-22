<script lang="ts">
    import DiamondBackdrop from "#lib/components/DiamondBackdrop.svelte";
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
    // The roster as shown: the player first, then everyone else in the
    // backend's order.
    let roster = $derived(
        team ? [...team.members].sort((a, b) => Number(b.id === user?.id) - Number(a.id === user?.id)) : [],
    );

    // Each member's avatar is a coloured block with their initial, the colour
    // picked from their id so it's the same on every phone.
    const AVATAR_COLOURS = ["bg-[#c0453a]", "bg-[#3a7bc0]", "bg-[#5fb04a]", "bg-[#9b59b6]", "bg-[#d98c2b]", "bg-[#2bb5a3]"];
    const avatarColour = (id: string) =>
        AVATAR_COLOURS[[...id].reduce((sum, c) => sum + c.charCodeAt(0), 0) % AVATAR_COLOURS.length];

    // Buttons look like the game menu's items: white outlined text, centred,
    // that a pink band highlights. The main action on screen is highlighted
    // already (menuMain); the rest light up on hover or focus.
    const menuItem =
        "block w-full px-4 py-2 text-center text-[16px] leading-[24px] text-white uppercase text-outline outline-none hover:bg-pink focus-visible:bg-pink active:translate-y-[2px] disabled:pointer-events-none disabled:opacity-40";
    const menuMain = `${menuItem} bg-pink`;

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

<div class="relative isolate flex min-h-dvh flex-col overflow-x-hidden pb-20 font-pixel text-[16px] leading-[24px] text-white">
    <DiamondBackdrop class="fixed inset-0 -z-10" />

    <!-- No visible title: the team's name (or the forms) heads the page. -->
    <h1 class="sr-only">Party</h1>

    <main class="mx-auto flex w-full max-w-md flex-col gap-8 px-5 pt-12">
        {#if form?.code}
            <div role="alert" class="pixel-box bg-danger p-4">
                <p class="uppercase text-outline">! Error</p>
                <p class="mt-2 text-[8px] leading-[16px] uppercase [overflow-wrap:anywhere]">
                    {form.action}: {form.code}: {form.message}
                </p>
            </div>
        {/if}

        {#if !team}
            <!-- No team: create one or join one by its code -->
            <form method="POST" action="?/create" class="flex flex-col gap-4">
                <h2 class="text-center font-display text-[32px] leading-[1] text-cyan uppercase heading-outline">New party</h2>
                <label class="flex flex-col gap-2">
                    <span class="text-[8px] leading-[16px] uppercase text-outline">Party name</span>
                    <input
                        name="name"
                        maxlength="32"
                        required
                        autocomplete="off"
                        placeholder="Seven Exes"
                        class="pixel-box bg-ink/80 px-3 py-3 text-white placeholder:text-white/30 focus:outline-4 focus:outline-offset-4 focus:outline-pink"
                    />
                </label>
                <button class={menuMain}>Create</button>
            </form>

            <p class="text-center text-[8px] leading-[16px] uppercase text-outline">- or -</p>

            <form method="POST" action="?/join" class="flex flex-col gap-4">
                <h2 class="text-center font-display text-[32px] leading-[1] text-cyan uppercase heading-outline">Join a party</h2>
                <label class="flex flex-col gap-2">
                    <span class="text-[8px] leading-[16px] uppercase text-outline">Party code</span>
                    <!-- The pattern is a JS string: written plainly, Svelte would
                         read {6} as an expression and render [0-9]6. -->
                    <input
                        name="team_id"
                        inputmode="numeric"
                        pattern={"[0-9]{6}"}
                        maxlength="6"
                        required
                        autocomplete="off"
                        placeholder="000000"
                        class="pixel-box bg-ink/80 px-3 py-3 text-center text-[24px] leading-[32px] tracking-[8px] text-white placeholder:text-white/30 focus:outline-4 focus:outline-offset-4 focus:outline-pink"
                    />
                </label>
                <button class={menuItem}>Join</button>
            </form>
        {:else}
            <!-- The team: its name, and the code to share -->
            <section class="flex flex-col items-center gap-4 text-center">
                <h2 class="font-display text-[40px] leading-[1.1] text-cyan uppercase heading-outline [overflow-wrap:anywhere]">{team.name}</h2>
                <div class="pixel-box w-full bg-ink/80 px-4 py-3">
                    <p class="text-[8px] leading-[16px] uppercase opacity-80">Party code</p>
                    <p class="mt-2 text-[32px] leading-[40px] tracking-[8px] text-pink">{team.id}</p>
                    {#if team.status === "LOBBY"}
                        <p class="mt-2 text-[8px] leading-[16px] uppercase opacity-80">Share it to recruit</p>
                    {/if}
                </div>
            </section>

            {#if team.status === "IN_GAME"}
                <a href="/play" class={menuMain}>Enter the labyrinth <span class="motion-safe:animate-blink">&gt;</span></a>
            {/if}

            <!-- The roster: one slot per member, then the empty ones -->
            <section class="flex flex-col gap-3">
                <div class="flex items-baseline justify-between">
                    <h2 class="font-display text-[32px] leading-[1] text-cyan uppercase heading-outline">Members</h2>
                    <span class="text-[8px] leading-[16px] uppercase text-outline">{team.members.length}/{PARTY_SIZE}</span>
                </div>

                <ul class="flex flex-col gap-3">
                    {#each roster as m (m.id)}
                        {@const leader = m.id === team.leader_id}
                        {@const isMe = m.id === user?.id}
                        <!-- The player's own row is first and lit up: a cyan
                             outline, a lighter box and their name in cyan. -->
                        <li
                            class="pixel-box flex items-center gap-3 p-2 {isMe
                                ? 'bg-[#0f2e66] outline-4 outline-offset-4 outline-cyan'
                                : 'bg-ink/80'}"
                        >
                            <span class="flex h-10 w-10 shrink-0 items-center justify-center text-ink {avatarColour(m.id)}">
                                {m.name.charAt(0).toUpperCase()}
                            </span>
                            <span class="flex min-w-0 flex-1 flex-col gap-1">
                                <span class="truncate {isMe ? 'text-cyan' : ''}">{m.name}</span>
                                <span class="flex flex-wrap gap-2 text-[8px] leading-[16px] uppercase">
                                    {#if leader}
                                        <span class="bg-pink px-1">Leader</span>
                                    {:else if m.is_ready}
                                        <span class="bg-go px-1 text-ink">Ready</span>
                                    {:else}
                                        <span class="bg-danger px-1">Not ready</span>
                                    {/if}
                                    {#if isMe}<span class="bg-cyan px-1 text-ink">You</span>{/if}
                                </span>
                            </span>
                        </li>
                    {/each}
                    {#each { length: emptySlots }, i (i)}
                        <li class="flex items-center gap-3 border-2 border-dashed border-white/30 p-2 text-white/50">
                            <span class="h-10 w-10 shrink-0 bg-ink/40"></span>
                            <span class="text-[8px] leading-[16px] uppercase">Empty slot</span>
                        </li>
                    {/each}
                </ul>
            </section>

            <!-- The lobby's actions, as a menu: the main one highlighted. -->
            {#if team.status === "LOBBY"}
                <nav class="flex flex-col items-stretch gap-1" aria-label="Party actions">
                    {#if isLeader}
                        <form method="POST" action="?/start">
                            <button disabled={!allReady} class={menuMain}>Start game</button>
                        </form>
                        <p class="py-1 text-center text-[8px] leading-[16px] uppercase text-outline">
                            {#if others.length === 0}
                                Solo run: start whenever
                            {:else if allReady}
                                Everyone's ready
                            {:else}
                                Waiting: {readyCount}/{others.length} ready
                            {/if}
                        </p>
                    {:else}
                        <form method="POST" action="?/ready">
                            <input type="hidden" name="ready" value={me?.is_ready ? "false" : "true"} />
                            <button class={me?.is_ready ? menuItem : menuMain}>{me?.is_ready ? "Unready" : "Ready up"}</button>
                        </form>
                    {/if}
                    <form method="POST" action="?/leave">
                        <button class="{menuItem} hover:bg-danger focus-visible:bg-danger">Leave party</button>
                    </form>
                </nav>
            {/if}
        {/if}
    </main>

    <!-- The button hints along the bottom: back home (left), the stream's
         state (right). -->
    <footer class="fixed inset-x-0 bottom-0 flex items-center justify-between bg-black/60 px-4 py-3 pb-[max(0.75rem,env(safe-area-inset-bottom))] text-[16px] uppercase">
        <a href="/" class="flex items-center gap-2 text-outline hover:text-pink">
            <span aria-hidden="true" class="flex h-6 w-6 items-center justify-center rounded-full border-2 border-ink bg-[#c7ccd8]">
                <span class="h-3 w-3 rounded-full border-2 border-danger"></span>
            </span>
            Back
        </a>
        <span class="flex items-center gap-2 text-[8px] leading-[16px] text-outline">
            {#if connected}
                <span class="h-2 w-2 bg-go motion-safe:animate-blink"></span> Live
            {:else}
                <span class="h-2 w-2 bg-danger"></span> Reconnecting
            {/if}
        </span>
    </footer>
</div>
