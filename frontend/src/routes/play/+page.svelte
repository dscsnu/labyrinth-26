<script lang="ts">
    import { ScanGate } from "#lib/game/scanGate.ts";
    import { ScanResultSchema, TeamStateSchema, type TeamState } from "#lib/types/Game.type.ts";
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

    // The countdown. The window comes from the backend (config.toml
    // game.scan_window) and never changes while the page is open. gate isn't
    // reactive, so version is bumped after each change.
    const gate = new ScanGate(untrack(() => data.game.scan_window_ms));
    let version = $state(0);
    let now = $state(performance.now());
    const observe = (s: TeamState, at = performance.now()) => {
        live = s;
        gate.observe(s.stack.length, at);
        version++;
    };

    let remaining = $derived.by(() => {
        void version;
        return gate.remainingMs(now);
    });
    let open = $derived.by(() => {
        void version;
        return gate.canScan(now);
    });
    let selected = $derived(ts.selection?.shape ?? null);
    let selectedShape = $derived(game.shapes.find((s) => s.id === selected));
    // Every scan is final: once you've scanned for this attempt, you're locked
    // until it ends (cast or restart). Wrong scans aren't marked, on purpose.
    let iScanned = $derived(ts.stack.some((s) => s.user_id === user?.id));
    // A stack as big as the type's largest pattern that hasn't cast must hold
    // a wrong scan: completion would have purged it. Only a restart helps.
    let stuck = $derived(!!selectedShape && selectedShape.points.length > 0 && ts.stack.length >= Math.max(...selectedShape.points));
    let busy = $state(false);
    let waypoint = $state("");
    let message = $state<{ ok: boolean; text: string } | null>(null);

    onMount(() => {
        // A stack already running when the page loaded: estimate how long ago
        // it started from the bottom scan's server time (wall clocks may
        // differ a little; the server's grace covers that).
        if (data.state.stack.length > 0) {
            const elapsed = Math.max(0, Date.now() - Date.parse(data.state.stack[0].scanned_at));
            observe(data.state, performance.now() - elapsed);
        }
        const tick = setInterval(() => (now = performance.now()), 100);
        const events = new EventSource("/team/events");
        events.onopen = () => (connected = true);
        events.onerror = () => (connected = false);
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
            ? { ok: true, text: "attempt restarted: every scan cleared, everyone can scan again" }
            : { ok: true, text: `selected ${shape}: stack purged` };
    };

    // Restarting is reselecting the same type: the backend purges every scan,
    // wrong ones included, and unlocks every member. Any member can do it.
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
        // The gate is the rule: after the window, nothing is sent.
        if (!gate.canScan(performance.now())) {
            message = { ok: false, text: "time's up: select the type again to restart" };
            return;
        }
        busy = true;
        const { res, json } = await call("/play/scan", "POST", { waypoint_id: waypoint.trim() });
        busy = false;
        if (!res.ok) return (message = { ok: false, text: `${json.code}: ${json.message}` });
        const result = ScanResultSchema.parse(json.payload);
        observe(result.state); // the first scan's 200 starts the countdown
        waypoint = "";
        message =
            result.outcome === "pattern_completed"
                ? { ok: true, text: `completed ${result.pattern?.join("-")}${result.already_completed ? " (again)" : ""}` }
                : { ok: true, text: "scan added" };
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
            {s.name} ({s.points.join("/")} pts · {s.patterns} patterns){ts.shapes.includes(s.id) ? " ✓" : ""}
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
        Every scan is final. If someone scans a point that doesn't fit, this attempt can't cast
        (you won't be told which scan it was): restart to clear every scan and try again.
    </small></p>
{:else}
    <p>No type selected.</p>
{/if}

<h2>2. Enter waypoints</h2>
<p>
    {#if remaining === null}
        Countdown starts when the first waypoint is accepted ({game.scan_window_ms / 1000} s).
    {:else if open}
        <b>{(remaining / 1000).toFixed(1)} s</b> left
    {:else}
        <b style="color: red">time's up</b>
        <button disabled={busy} onclick={restart}>restart attempt</button>
    {/if}
</p>
{#if stuck}
    <p style="color: red">
        Everyone needed has scanned and nothing cast, so a scan didn't fit.
        <button disabled={busy} onclick={restart}>restart attempt</button>
    </p>
{/if}
{#if selected && iScanned}
    <p>You've scanned for this attempt; a teammate scans the next point. Locked until it casts or someone restarts.</p>
{/if}
<form onsubmit={scan}>
    <input bind:value={waypoint} inputmode="numeric" pattern="[0-9]{4}" maxlength="4" placeholder="1001" required
        disabled={!selected || !open || iScanned || busy} />
    <button disabled={!selected || !open || iScanned || busy}>Scan</button>
    <small>dev seed waypoints: 1001–1011</small>
</form>

{#if message}
    <p style:color={message.ok ? "green" : "red"}>{message.text}</p>
{/if}

{#if ts.last_cast}
    <p>
        Last cast: <b>{ts.last_cast.shape}</b> {ts.last_cast.pattern.join("-")} by {memberName(ts.last_cast.cast_by)}
        at {new Date(ts.last_cast.cast_at).toLocaleTimeString()}{ts.last_cast.already_completed ? " (cast before)" : ""}
    </p>
{/if}

<h3>Stack (bottom → top)</h3>
{#if ts.stack.length === 0}
    <p>empty</p>
{:else}
    <ol>
        {#each ts.stack as s (s.waypoint_id)}
            <li>{s.waypoint_id} by {memberName(s.user_id)}{s.user_id === user?.id ? " (you)" : ""}</li>
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
