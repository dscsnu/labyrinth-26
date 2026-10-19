<script lang="ts">
    import { ServerClock } from "#lib/game/serverClock.ts";
    import { ScanResultSchema, TeamStateSchema, type Cast, type TeamState } from "#lib/types/Game.type.ts";
    import { TeamSchema, type Team } from "#lib/types/Team.type.ts";
    import { goto } from "$app/navigation";
    import { onMount, untrack } from "svelte";
    import type { PageProps } from "./$types";

    let { data }: PageProps = $props();
    let { user, game } = $derived(data);

    // Latest state from responses and events; until the first, the page load's.
    let live = $state<TeamState | undefined>(undefined);
    let ts = $derived(live ?? data.state);
    let team = $state<Team | null>(null);
    let connected = $state(false);

    // The cast log, newest first: loaded with the page, then each new
    // last_cast (from a response or team_state) is prepended once.
    let casts = $state<Cast[]>(untrack(() => data.casts));
    const observe = (s: TeamState) => {
        live = s;
        const lc = s.last_cast;
        if (lc && !casts.some((c) => c.cast_at === lc.cast_at)) casts = [lc, ...casts];
    };

    // Time, measured the one canonical way: the server's clock as estimated
    // from the stream's hello event (ServerClock). There's no time limit; it
    // only labels scans "how long ago", the same on every phone.
    const clock = new ServerClock();
    let now = $state(clock.now());
    const ago = (iso: string) => {
        const s = Math.max(0, Math.round((now - Date.parse(iso)) / 1000));
        return s < 60 ? `${s} s ago` : s < 3600 ? `${Math.floor(s / 60)} min ago` : `${Math.floor(s / 3600)} h ago`;
    };

    let selected = $derived(ts.selection?.shape ?? null);
    let selectedShape = $derived(game.shapes.find((s) => s.id === selected));
    let busy = $state(false);
    let waypoint = $state("");
    let message = $state<{ ok: boolean; text: string } | null>(null);

    onMount(() => {
        const tick = setInterval(() => (now = clock.now()), 1000);
        const events = new EventSource("/api/events");
        events.onopen = () => (connected = true);
        events.onerror = () => (connected = false);
        // Every (re)connect opens with hello: re-measure the clock offset.
        events.addEventListener("hello", (e) => {
            clock.hello(JSON.parse(e.data).server_time);
            now = clock.now();
        });
        events.addEventListener("team", (e) => (team = TeamSchema.parse(JSON.parse(e.data))));
        events.addEventListener("team_state", (e) => observe(TeamStateSchema.parse(JSON.parse(e.data))));
        // /play needs a team: without one (never had one, or it's gone) the
        // player belongs on /team, where they can create or join one.
        events.addEventListener("no_team", () => {
            events.close();
            goto("/team");
        });
        return () => {
            clearInterval(tick);
            events.close();
        };
    });

    const call = async (path: string, method: string, body?: unknown) => {
        const res = await fetch(path, {
            method,
            headers: { "Content-Type": "application/json" },
            body: body === undefined ? undefined : JSON.stringify(body),
        });
        return { res, json: await res.json().catch(() => ({})) };
    };

    const selectShape = async (shape: string, restart = false) => {
        busy = true;
        const { res, json } = await call("/play/selection", "PUT", { shape });
        busy = false;
        if (!res.ok) return (message = { ok: false, text: `${json.code}: ${json.message}` });
        observe(TeamStateSchema.parse(json.payload));
        message = restart
            ? { ok: true, text: "attempt restarted: every scan cleared" }
            : { ok: true, text: `selected ${shape}: stack purged` };
    };

    // Restarting is reselecting the same type: the backend purges every scan,
    // the wrong one included. Any member can do it.
    const restart = () => selected && selectShape(selected, true);

    const giveUp = async () => {
        busy = true;
        const { res, json } = await call("/play/selection", "DELETE");
        busy = false;
        if (!res.ok) return (message = { ok: false, text: `${json.code}: ${json.message}` });
        observe(TeamStateSchema.parse(json.payload));
        message = { ok: true, text: "selection cleared" };
    };

    const scan = async (e: SubmitEvent) => {
        e.preventDefault();
        busy = true;
        const { res, json } = await call("/play/scan", "POST", { waypoint_id: waypoint.trim() });
        busy = false;
        if (!res.ok) return (message = { ok: false, text: `${json.code}: ${json.message}` });
        const result = ScanResultSchema.parse(json.payload);
        observe(result.state);
        const name = selectedShape?.name ?? selected;
        message =
            result.outcome === "pattern_completed"
                ? { ok: true, text: `completed ${result.pattern?.join("-")}${result.already_completed ? " (again)" : ""}` }
                : result.outcome === "wrong_scan"
                  ? { ok: false, text: `${waypoint.trim()} doesn't match any ${name} pattern with the points already scanned. Restart the attempt.` }
                  : { ok: true, text: "scan added" };
        waypoint = "";
    };

    const memberName = (id: string) => team?.members.find((m: Team["members"][number]) => m.id === id)?.name ?? id.slice(0, 8);
</script>

<p>
    <a href="/">home</a> · <a href="/team">team</a> · {user?.name} ·
    {connected ? "live ●" : "reconnecting…"}
</p>

{#if team && team.status !== "IN_GAME"}
    <p>Your team hasn't started yet. <a href="/team">Back to the lobby</a>.</p>
{/if}

<h1>Play</h1>

<h2>1. Pick a type</h2>
<p>
    {#each game.shapes as s (s.id)}
        <button disabled={busy} onclick={() => selectShape(s.id)} style:font-weight={s.id === selected ? "bold" : "normal"}>
            {s.name} ({s.points.join("/")} pts · {s.damage.min}–{s.damage.max} dmg · {s.patterns} patterns){ts.shapes.includes(s.id) ? " ✓" : ""}
        </button>
    {/each}
</p>
{#if selected}
    <p>
        Selected: <b>{selectedShape?.name ?? selected}</b>
        <button disabled={busy} onclick={restart}>restart attempt</button>
        <button disabled={busy} onclick={giveUp}>give up</button>
    </p>
    <p><small>
        No time limit, and anyone can scan any of the points. A point that doesn't fit any
        {selectedShape?.name ?? selected} pattern spoils the attempt: restart to clear it.
    </small></p>
{:else}
    <p>No type selected.</p>
{/if}

<h2>2. Enter waypoints</h2>
{#if ts.poisoned}
    <p style="color: red">
        A scan doesn't match any {selectedShape?.name ?? selected} pattern, so this attempt can't cast.
        <button disabled={busy} onclick={restart}>restart attempt</button>
    </p>
{/if}
<form onsubmit={scan}>
    <input bind:value={waypoint} inputmode="numeric" pattern="[0-9]{4}" maxlength="4" placeholder="1001" required
        disabled={!selected || ts.poisoned || busy} />
    <button disabled={!selected || ts.poisoned || busy}>Scan</button>
    <small>dev seed waypoints: 1001–1011</small>
</form>

{#if message}
    <p style:color={message.ok ? "green" : "red"}>{message.text}</p>
{/if}


<h3>Stack (bottom → top)</h3>
{#if ts.stack.length === 0}
    <p>empty</p>
{:else}
    <ol>
        {#each ts.stack as s (s.waypoint_id)}
            <li style:color={s.wrong ? "red" : null}>
                {s.waypoint_id} by {memberName(s.user_id)}{s.user_id === user?.id ? " (you)" : ""}, {ago(s.scanned_at)}{s.wrong ? " ✗ doesn't fit" : ""}
            </li>
        {/each}
    </ol>
{/if}

<h2>Casts</h2>
{#if casts.length === 0}
    <p>none yet</p>
{:else}
    <ol>
        {#each casts as c (c.cast_at)}
            <li>
                <b>{c.shape}</b> {c.pattern.join("-")} ({c.damage} dmg) by {memberName(c.cast_by)},
                {new Date(c.cast_at).toLocaleTimeString()}{c.already_completed ? " (cast before)" : ""}
            </li>
        {/each}
    </ol>
{/if}

<h2>Progress</h2>
<p>Types completed: {ts.shapes.length ? ts.shapes.join(", ") : "none"}</p>
<p>Patterns completed: {ts.patterns.length}</p>
<ul>
    {#each ts.patterns as p (p.join("-"))}<li>{p.join("-")}</li>{/each}
</ul>
<p>Waypoints visited: {ts.waypoints.join(", ") || "none"}</p>
