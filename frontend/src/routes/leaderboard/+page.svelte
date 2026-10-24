<script lang="ts">
    import DiamondBackdrop from "#lib/components/DiamondBackdrop.svelte";
    import { envelope } from "#lib/types/Live.type.ts";
    import { LeaderboardBoardSchema, type LeaderboardBoard, type LeaderboardEntry } from "#lib/types/Leaderboard.type.ts";
    import fitText from "#lib/utils/fitText.ts";
    import { onMount } from "svelte";
    import type { PageProps } from "./$types";

    let { data }: PageProps = $props();

    // The top follows the game-wide "leaderboard" event (sent on connect and
    // whenever the top changes); until the first, the page load's. The
    // player's own entry is the page load's: reload to refresh it.
    let live = $state<LeaderboardBoard | null>(null);
    let board = $derived(live ?? data.board);
    let connected = $state(false);

    onMount(() => {
        const events = new EventSource("/api/events");
        events.onopen = () => (connected = true);
        events.onerror = () => (connected = false);
        events.addEventListener("leaderboard", (e) => {
            live = envelope(LeaderboardBoardSchema).parse(JSON.parse(e.data)).message;
        });
        return () => events.close();
    });

    const when = (iso: string | null) => (iso ? new Date(iso).toLocaleTimeString() : null);
    const isMe = (e: LeaderboardEntry) => e.team_id === data.board.me?.team_id;
    let meInTop = $derived(board.top.some(isMe));

    // The player's team sits on the black band, like the title screen's
    // selected item, its name repeated faded either side.
    const BAND_SIDE_REPEATS = 3;

    // Every row's columns, the headings' too, so they line up: the rank
    // wide enough for "#999", the name taking the rest, the score.
    const columns = "grid w-full max-w-md shrink-0 grid-cols-[4rem_1fr_auto] items-center gap-3 px-4";
</script>

<svelte:head><title>Leaderboard · Labyrinth</title></svelte:head>

<!-- One team's row: rank, its whole name (wrapping between words, shrunk
     until its longest word fits, never cut short), and
     its score (bosses, then damage, then when its last boss fell, each on
     its own line to leave the name room). The
     player's own is on the black band. -->
{#snippet row(e: LeaderboardEntry)}
    {@const me = isMe(e)}
    {@const fell = when(e.last_defeat_at)}
    <li class="flex justify-center gap-10 overflow-hidden whitespace-nowrap uppercase {me ? 'bg-black py-1' : 'py-1'}">
        {#if me}
            {#each { length: BAND_SIDE_REPEATS }, r (r)}
                <span aria-hidden="true" class="shrink-0 self-center text-tile-light">{e.name}</span>
            {/each}
        {/if}
        <div class={columns}>
            <span class="text-outline {e.rank === 1 ? 'text-gold-light' : ''}">#{e.rank}</span>
            <span class="flex min-w-0 items-center gap-2">
                <span
                    {@attach fitText({ max: 16, min: 8 })}
                    class="min-w-0 px-1 leading-[1.5] whitespace-normal {me ? 'bg-pink' : 'text-outline'}"
                >{e.name}</span>
                {#if e.finished}<span aria-label="finished" class="shrink-0">🏆</span>{/if}
                {#if me}<span class="shrink-0 bg-cyan px-1 text-[8px] leading-[16px] text-ink">You</span>{/if}
            </span>
            <span class="flex flex-col items-end text-outline">
                <span>{e.bosses_defeated} <span aria-label="bosses defeated">⚔</span></span>
                <span class="text-[8px] leading-[16px]">{e.damage} dmg</span>
                {#if fell}<span class="text-[8px] leading-[16px]">{fell}</span>{/if}
            </span>
        </div>
        {#if me}
            {#each { length: BAND_SIDE_REPEATS }, r (r)}
                <span aria-hidden="true" class="shrink-0 self-center text-tile-light">{e.name}</span>
            {/each}
        {/if}
    </li>
{/snippet}

<div class="relative isolate flex min-h-dvh flex-col overflow-x-hidden pb-20 font-pixel text-[16px] leading-[24px] text-white">
    <DiamondBackdrop class="fixed inset-0 -z-10" />

    <main class="flex flex-1 flex-col gap-8 py-12">
        <header class="flex flex-col items-center px-5 text-center">
            <h1 class="font-display text-[clamp(32px,10vw,64px)] leading-[1] text-cyan uppercase logo-outline">Leaderboard</h1>
            <p class="mt-6 uppercase text-outline">{board.teams} {board.teams === 1 ? "team" : "teams"} playing</p>
            <p class="mt-3 text-[8px] leading-[16px] uppercase text-outline">Bosses, then who got there first, then damage</p>
        </header>

        {#if board.top.length > 0}
            <!-- The columns' headings, in the rows' grid, on a dark strip in
                 the logo's cyan: what the score column's two lines are
                 under its heading. Each value is labelled already, so
                 screen readers skip them. -->
            <div class="flex flex-col gap-2">
                <div aria-hidden="true" class="flex justify-center border-y-4 border-black bg-ink/80 py-2 uppercase">
                    <div class={columns}>
                        <span class="font-display text-[24px] leading-[1] text-cyan heading-outline">Rank</span>
                        <span class="px-1 font-display text-[24px] leading-[1] text-cyan heading-outline">Team</span>
                        <span class="flex flex-col items-end gap-1">
                            <span class="font-display text-[24px] leading-[1] text-cyan heading-outline">Bosses</span>
                            <span class="flex flex-col items-end text-[8px] leading-[16px] text-outline">
                                <span>Dmg</span>
                                <span>Last kill</span>
                            </span>
                        </span>
                    </div>
                </div>
                <ol aria-label="Leaderboard" class="flex flex-col gap-1">
                    {#each board.top as e (e.team_id)}
                        {@render row(e)}
                    {/each}
                </ol>
            </div>
        {:else}
            <p class="text-center text-[8px] leading-[16px] uppercase text-outline">No teams in the game yet.</p>
        {/if}

        <!-- The player's team, when it's outside the top. -->
        {#if data.board.me && !meInTop}
            <section aria-label="Your team" class="flex flex-col gap-2">
                <p class="text-center text-[8px] leading-[16px] uppercase text-outline">- - - Your team - - -</p>
                <ol class="flex flex-col">
                    {@render row(data.board.me)}
                </ol>
            </section>
        {/if}
    </main>

    <!-- The bottom bar: back to the title screen, and whether the top is live. -->
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
