<script lang="ts">
    import { envelope } from "#lib/types/Live.type.ts";
    import { LeaderboardBoardSchema, type LeaderboardBoard } from "#lib/types/Leaderboard.type.ts";
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

    const when = (iso: string | null) => (iso ? new Date(iso).toLocaleTimeString() : "—");
</script>

<p><a href="/">home</a> · <a href="/play">play</a> · {connected ? "live ●" : "reconnecting…"}</p>

<h1>Leaderboard</h1>
<p><small>
    Ranked by bosses defeated, then who got there first, then damage on the current boss.
    {board.teams} teams playing.
</small></p>

<table>
    <thead>
        <tr><th>#</th><th>team</th><th>bosses</th><th>last boss fell</th><th>damage</th></tr>
    </thead>
    <tbody>
        {#each board.top as e (e.team_id)}
            <tr style:font-weight={e.team_id === data.board.me?.team_id ? "bold" : "normal"}>
                <td>{e.rank}</td>
                <td>{e.name}{e.finished ? " 🏆" : ""}</td>
                <td>{e.bosses_defeated}</td>
                <td>{when(e.last_defeat_at)}</td>
                <td>{e.damage}</td>
            </tr>
        {/each}
    </tbody>
</table>

{#if data.board.me && !board.top.some((e) => e.team_id === data.board.me?.team_id)}
    <p>Your team: <b>#{data.board.me.rank}</b> {data.board.me.name}, {data.board.me.bosses_defeated} bosses, {data.board.me.damage} damage</p>
{/if}
