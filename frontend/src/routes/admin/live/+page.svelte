<script lang="ts">
    import { ActivityBatchSchema, AdminLeaderboardSchema, type ActivityEntry } from "#lib/types/Admin.type.ts";
    import type { LeaderboardEntry } from "#lib/types/Leaderboard.type.ts";
    import { envelope } from "#lib/types/Live.type.ts";
    import { MAP_POINTS } from "#lib/game/mapPoints.ts";
    import { onMount } from "svelte";
    import type { PageProps } from "./$types";

    let { data }: PageProps = $props();

    // The feed, newest first, as lines: an entry, or a note that a batch left
    // some out. At most KEEP, so a long session doesn't grow the page.
    const KEEP = 500;
    type Line = { kind: "entry"; entry: ActivityEntry; key: number } | { kind: "dropped"; count: number; key: number };
    let lines = $state<Line[]>([]);
    let ranking = $state<LeaderboardEntry[]>([]);
    let connected = $state(false);
    let filter = $state("");
    // Paused: new lines wait in held, shown on resume.
    let paused = $state(false);
    let held: Line[] = [];
    let heldCount = $state(0);
    let nextKey = 0;

    // Adds a batch's entries (oldest first) to the top of the feed.
    const add = (entries: ActivityEntry[], dropped: number) => {
        const fresh: Line[] = entries.map((entry) => ({ kind: "entry", entry, key: nextKey++ }));
        if (dropped > 0) fresh.push({ kind: "dropped", count: dropped, key: nextKey++ });
        fresh.reverse();
        if (paused) {
            held = [...fresh, ...held].slice(0, KEEP);
            heldCount = held.length;
            return;
        }
        lines = [...fresh, ...lines].slice(0, KEEP);
    };
    const resume = () => {
        lines = [...held, ...lines].slice(0, KEEP);
        held = [];
        heldCount = 0;
        paused = false;
    };

    onMount(() => {
        // ?admin=1: the backend adds the live view to an admin's stream.
        const events = new EventSource("/api/events?admin=1");
        events.onopen = () => (connected = true);
        events.onerror = () => (connected = false);
        events.addEventListener("activity", (e) => {
            const { type, message } = envelope(ActivityBatchSchema).parse(JSON.parse(e.data));
            // A snapshot is the recent entries, sent on every (re)connect:
            // it replaces the feed rather than repeating it.
            if (type === "snapshot") {
                lines = [];
                held = [];
                heldCount = 0;
            }
            add(message.entries, message.dropped);
        });
        events.addEventListener("admin_leaderboard", (e) => {
            ranking = envelope(AdminLeaderboardSchema).parse(JSON.parse(e.data)).message.teams;
        });
        return () => events.close();
    });

    const bossName = (serial?: number) =>
        serial === undefined ? "" : (data.bosses.find((b) => b.serial === serial)?.name ?? `boss ${serial}`);
    const place = (id?: string) => {
        const p = MAP_POINTS.find((m) => m.waypoint === id);
        return p ? `${p.name} (${id})` : (id ?? "");
    };
    const time = (iso: string) => new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
    const who = (e: ActivityEntry) => e.by ?? "someone";

    // One readable line per kind of change.
    const describe = (e: ActivityEntry): string => {
        switch (e.type) {
            case "game_started": return "started the game";
            case "shape_selected": return `${who(e)} selected ${e.shape}`;
            case "selection_cleared": return "cleared the selection";
            case "scan_added": return `${who(e)} scanned ${place(e.waypoint)}`;
            case "wrong_scan": return `${who(e)} scanned ${place(e.waypoint)}: doesn't fit`;
            case "spell_cast": return `${who(e)} cast ${e.shape}: ${e.dealt} dmg to ${bossName(e.boss)}`;
            case "boss_defeated": return `${who(e)} cast ${e.shape}: ${e.dealt} dmg, ${bossName(e.boss)} defeated!`;
            case "boss_attacked": return `${bossName(e.boss)} attacked: health ${e.health}`;
            case "knocked_out": return `knocked out by ${bossName(e.boss)}`;
            case "revived": return `revived: health ${e.health}`;
            case "powerup_granted": return `got ${e.powerup || "a powerup"}`;
            case "powerup_activated": return `used ${e.powerup || "a powerup"}`;
            case "powerup_ended": return "a powerup ran out";
            case "qte_answered": return `${who(e)} answered the quiz: ${e.correct ? "right" : "wrong"}`;
            case "qr_cooldown_ended": return "QR cooldown ended";
            case "cooldowns_reset": return "cooldowns reset by an organiser";
            default: return e.type;
        }
    };
    const tone = (type: string) =>
        type === "knocked_out" || type === "wrong_scan" ? "bad"
        : type === "boss_defeated" || type === "revived" ? "good"
        : type === "spell_cast" ? "cast" : "";

    let shown = $derived.by(() => {
        const q = filter.trim().toLowerCase();
        if (!q) return lines;
        return lines.filter((l) => l.kind === "entry" &&
            (l.entry.team.toLowerCase().includes(q) || l.entry.team_id.includes(q) || (l.entry.by ?? "").toLowerCase().includes(q)));
    });
</script>

<svelte:head><title>Live · Admin · Labyrinth</title></svelte:head>

<main>
    <p>
        <a href="/">home</a> · <a href="/admin">admin</a> · live view ·
        <span class:live={connected}>{connected ? "live ●" : "reconnecting…"}</span>
    </p>

    <div class="grid">
        <section>
            <h1>Leaderboard</h1>
            {#if ranking.length === 0}
                <p>No team has started yet.</p>
            {:else}
                <table>
                    <thead>
                        <tr><th>#</th><th>Team</th><th>Bosses</th><th>Damage</th><th>Last boss down</th></tr>
                    </thead>
                    <tbody>
                        {#each ranking as t (t.team_id)}
                            <tr>
                                <td>{t.rank}</td>
                                <td>{t.name} <small>{t.team_id}</small>{#if t.finished} 🏆{/if}</td>
                                <td>{t.bosses_defeated}</td>
                                <td>{t.damage}</td>
                                <td>{t.last_defeat_at ? time(t.last_defeat_at) : "–"}</td>
                            </tr>
                        {/each}
                    </tbody>
                </table>
            {/if}
        </section>

        <section>
            <h1>Activity</h1>
            <p class="controls">
                <input bind:value={filter} placeholder="filter by team, code or player" size="28" />
                {#if paused}
                    <button onclick={resume}>resume{heldCount ? ` (${heldCount} new)` : ""}</button>
                {:else}
                    <button onclick={() => (paused = true)}>pause</button>
                {/if}
            </p>
            {#if shown.length === 0}
                <p>Nothing yet. Every selection, scan, cast, attack and powerup appears here as it happens.</p>
            {:else}
                <ol>
                    {#each shown as l (l.key)}
                        {#if l.kind === "dropped"}
                            <li class="dropped">+{l.count} more not shown (too many at once)</li>
                        {:else}
                            <li class={tone(l.entry.type)}>
                                <time>{time(l.entry.at)}</time>
                                <b>{l.entry.team}</b>
                                {describe(l.entry)}
                            </li>
                        {/if}
                    {/each}
                </ol>
            {/if}
        </section>
    </div>
</main>

<!-- /admin's plain, readable style: Tailwind's reset strips the browser's
     defaults, so they're put back here for this page only. -->
<style>
    main {
        max-width: 80rem;
        margin: 0 auto;
        padding: 1rem;
        font: 15px/1.5 system-ui, sans-serif;
        color: #1f2328;
    }
    :global(body:has(main)) {
        background: #f6f7f9;
    }
    h1 {
        margin: 0 0 0.75rem;
        font-size: 1.25rem;
        font-weight: 700;
    }
    a {
        color: #0969da;
        text-decoration: underline;
    }
    p {
        margin: 0.5rem 0;
    }
    small {
        color: #656d76;
    }
    .live {
        color: #1a7f37;
    }
    .grid {
        display: grid;
        grid-template-columns: minmax(0, 2fr) minmax(0, 3fr);
        gap: 1rem;
        align-items: start;
    }
    @media (max-width: 800px) {
        .grid {
            grid-template-columns: minmax(0, 1fr);
        }
    }
    section {
        padding: 0.75rem 1rem;
        background: white;
        border: 1px solid #d0d7de;
        border-radius: 8px;
    }
    table {
        width: 100%;
        border-collapse: collapse;
    }
    th,
    td {
        padding: 0.25rem 0.5rem;
        border-bottom: 1px solid #eaeef2;
        text-align: left;
    }
    th {
        font-weight: 600;
    }
    .controls {
        display: flex;
        gap: 0.5rem;
        flex-wrap: wrap;
    }
    input,
    button {
        padding: 0.25rem 0.5rem;
        background: white;
        border: 1px solid #8c959f;
        border-radius: 6px;
    }
    button {
        cursor: pointer;
    }
    ol {
        margin: 0;
        padding: 0;
        list-style: none;
        max-height: 75vh;
        overflow-y: auto;
    }
    li {
        padding: 0.2rem 0;
        border-bottom: 1px solid #f0f2f4;
    }
    time {
        color: #656d76;
        font-variant-numeric: tabular-nums;
        margin-right: 0.5rem;
    }
    li.bad {
        color: #cf222e;
    }
    li.good {
        color: #1a7f37;
        font-weight: 600;
    }
    li.cast {
        color: #8250df;
    }
    li.dropped {
        color: #656d76;
        font-style: italic;
    }
</style>
