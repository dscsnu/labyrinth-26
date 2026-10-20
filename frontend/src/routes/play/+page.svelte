<script lang="ts">
  import MapView, { ZOOM_LEVELS } from "#lib/components/MapView.svelte";
  import SpellBook from "#lib/components/SpellBook.svelte";
  import qrSprite from "#lib/assets/UserInterfaceButtons/qr-sprite.webp";
  import zoomInIcon from "#lib/assets/UserInterfaceButtons/zoom-in-icon.webp";
  import zoomOutIcon from "#lib/assets/UserInterfaceButtons/zoom-out-icon.webp";
  import bookIcon from "#lib/assets/UserInterfaceButtons/book-icon.webp";
  import { ServerClock } from "#lib/game/serverClock.ts";
  import {
    CastLogSchema,
    ScanResultSchema,
    TeamStateSchema,
    type Cast,
    type TeamState,
  } from "#lib/types/Game.type.ts";
  import { envelope, isNewer } from "#lib/types/Live.type.ts";
  import { TeamSchema, type Team } from "#lib/types/Team.type.ts";
  import { z } from "zod";
  import { goto } from "$app/navigation";
  import { onMount, untrack } from "svelte";
  import type { PageProps } from "./$types";

  let { data }: PageProps = $props();

  // The map's zoom step (an index into ZOOM_LEVELS), moved by the zoom buttons.
  let zoomLevel = $state(0);
  // Whether the spell book (the book button) is open.
  let bookOpen = $state(false);
  let { user, game, bosses } = $derived(data);

  // Latest state from responses and events; until the first, the page load's.
  let live = $state<TeamState | undefined>(undefined);
  let ts = $derived(live ?? data.state);
  let team = $state<Team | null>(null);
  let connected = $state(false);

  // The cast log, newest first: loaded with the page, then each new
  // last_cast (from a response or team_state) is prepended once. When
  // cast_count says the log is longer than ours (missed while
  // disconnected), it's fetched again.
  let casts = $state<Cast[]>(untrack(() => data.casts));
  let refetching = false;
  const refetchCasts = async () => {
    if (refetching) return;
    refetching = true;
    const res = await fetch("/play/casts").catch(() => null);
    refetching = false;
    if (res?.ok) casts = CastLogSchema.parse((await res.json()).payload);
  };

  // Every copy of the state, from the stream or a response, goes through
  // here: it's kept only if newer than the one shown (by version; they can
  // arrive in either order).
  const observe = (s: TeamState) => {
    if (!isNewer(ts.version, s.version)) return;
    live = s;
    const lc = s.last_cast;
    if (lc && !casts.some((c) => c.cast_at === lc.cast_at))
      casts = [lc, ...casts];
    if (s.cast_count > casts.length) refetchCasts();
  };

  // The boss being fought, from the static list by the state's serial, or
  // null once every boss is defeated (or none are loaded).
  let boss = $derived(
    ts.boss ? (bosses.find((b) => b.serial === ts.boss!.serial) ?? null) : null,
  );
  const bossName = (serial: number | null) =>
    bosses.find((b) => b.serial === serial)?.name ?? `boss ${serial}`;

  // The countdown to the boss's next attack, on the server's clock. The
  // server applies the attack and publishes it; this only displays.
  const countdown = (iso: string | null) => {
    if (!iso) return null;
    const s = Math.max(0, Math.ceil((Date.parse(iso) - now) / 1000));
    return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
  };

  // Time, measured the one canonical way: the server's clock as estimated
  // from the stream's hello event (ServerClock). There's no time limit; it
  // only labels scans "how long ago", the same on every phone.
  const clock = new ServerClock();
  let now = $state(clock.now());
  const ago = (iso: string) => {
    const s = Math.max(0, Math.round((now - Date.parse(iso)) / 1000));
    return s < 60
      ? `${s} s ago`
      : s < 3600
        ? `${Math.floor(s / 60)} min ago`
        : `${Math.floor(s / 3600)} h ago`;
  };

  let selected = $derived(ts.selection?.shape ?? null);
  // Only the leader picks a type, restarts or gives up; everyone scans.
  let isLeader = $derived(!!team && team.leader_id === user?.id);
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
    // Every event's data is {type, message}: type for moments, message
    // for state.
    events.addEventListener("hello", (e) => {
      clock.hello(
        envelope(z.object({ server_time: z.string() })).parse(
          JSON.parse(e.data),
        ).message.server_time,
      );
      now = clock.now();
    });
    events.addEventListener("team", (e) => {
      const next = envelope(TeamSchema).parse(JSON.parse(e.data)).message;
      if (next.id !== team?.id || isNewer(team.version, next.version))
        team = next;
    });
    events.addEventListener("team_state", (e) => {
      const { type, message: next } = envelope(TeamStateSchema).parse(
        JSON.parse(e.data),
      );
      const fresh = isNewer(ts.version, next.version);
      observe(next);
      // Moments, on every phone: a teammate's cast that finished a boss,
      // and the boss's attacks.
      if (!fresh) return;
      if (type === "boss_defeated" && next.last_cast) {
        message = {
          ok: true,
          text: `${bossName(next.last_cast.boss)} defeated!`,
        };
      } else if (type === "boss_attacked" && next.boss) {
        message = {
          ok: false,
          text: `${bossName(next.boss.serial)} attacked! Health ${next.health}/${game.health}`,
        };
      } else if (type === "knocked_out") {
        message = {
          ok: false,
          text: "Knocked out! Ask an organiser to revive your team.",
        };
      } else if (type === "revived") {
        message = { ok: true, text: "Revived: full health" };
      }
    });
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
    if (!res.ok)
      return (message = { ok: false, text: `${json.code}: ${json.message}` });
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
    if (!res.ok)
      return (message = { ok: false, text: `${json.code}: ${json.message}` });
    observe(TeamStateSchema.parse(json.payload));
    message = { ok: true, text: "selection cleared" };
  };

  const scan = async (e: SubmitEvent) => {
    e.preventDefault();
    busy = true;
    const { res, json } = await call("/play/scan", "POST", {
      waypoint_id: waypoint.trim(),
    });
    busy = false;
    if (!res.ok)
      return (message = { ok: false, text: `${json.code}: ${json.message}` });
    const result = ScanResultSchema.parse(json.payload);
    observe(result.state);
    const name = selectedShape?.name ?? selected;
    const lc = result.state.last_cast;
    message =
      result.outcome === "pattern_completed"
        ? {
            ok: true,
            text:
              `completed ${result.pattern?.join("-")}${result.already_completed ? " (again)" : ""}` +
              (lc && lc.boss !== null
                ? `: ${lc.dealt} dmg to ${bossName(lc.boss)}${lc.defeated_boss ? ", defeated!" : ""}`
                : ""),
          }
        : result.outcome === "wrong_scan"
          ? {
              ok: false,
              text: `${waypoint.trim()} doesn't match any ${name} pattern with the points already scanned. Restart the attempt.`,
            }
          : { ok: true, text: "scan added" };
    waypoint = "";
  };

  const memberName = (id: string) =>
    team?.members.find((m: Team["members"][number]) => m.id === id)?.name ??
    id.slice(0, 8);
</script>

<!-- Top bar: full width, the top 20% of the screen. The current boss's image,
     name and health, from ts, so it moves on every teammate's team_state. -->
<div
  class="fixed inset-x-0 top-0 flex h-[20dvh] items-center gap-4 bg-brown px-4 py-2 font-pixel text-[16px] leading-[24px] text-parchment"
>
  {#if boss && ts.boss}
    {@const hp = Math.max(0, boss.hp - ts.boss.damage)}
    <!-- The boss's image (static/<asset>), as tall as the bar; hidden if
             the file is missing, leaving the name and health. Keyed by serial
             so the next boss's image never inherits the last one's hiding. -->
    {#key boss.serial}
      <img
        src="/{boss.asset}"
        alt=""
        width="628"
        height="802"
        draggable="false"
        onerror={(e) => ((e.currentTarget as HTMLImageElement).hidden = true)}
        class="h-full w-auto shrink-0 object-contain"
      />
    {/key}
    <div class="flex min-w-0 flex-1 flex-col justify-center gap-2">
      <p class="truncate uppercase">{boss.name}</p>
      <div
        role="progressbar"
        aria-label="{boss.name}'s health"
        aria-valuemin={0}
        aria-valuemax={boss.hp}
        aria-valuenow={hp}
        class="pixel-box mx-1 h-4 bg-ink"
      >
        <div
          class="h-full bg-danger transition-[width] duration-300"
          style:width="{(hp / boss.hp) * 100}%"
        ></div>
      </div>
      <p class="text-[8px] leading-[16px]">{hp} / {boss.hp} HP</p>
    </div>
  {:else if bosses.length > 0}
    <p class="uppercase">Every boss defeated!</p>
  {:else}
    <p class="uppercase">No bosses loaded.</p>
  {/if}
</div>

<!-- The map: the rest of the screen, below the bar. -->
<div class="fixed inset-x-0 top-[20dvh] bottom-0">
  <MapView bind:level={zoomLevel} />

  <!-- QR scanner button, bottom left. The scanner isn't built yet, so it does nothing. -->
  <button
    type="button"
    aria-label="Scan QR code"
    class="absolute bottom-4 left-4 h-20 w-20 rounded-full transition-transform active:scale-90"
  >
    <img
      src={qrSprite}
      alt=""
      width="128"
      height="128"
      draggable="false"
      class="h-full w-full [image-rendering:pixelated]"
    />
  </button>

  <!-- Map controls, bottom right, in a column. Zoom steps through
         ZOOM_LEVELS, disabled at either end; the book isn't built yet. -->
  <div class="absolute right-4 bottom-4 flex flex-col items-center gap-3">
    <button
      type="button"
      aria-label="Zoom in"
      disabled={zoomLevel >= ZOOM_LEVELS.length - 1}
      onclick={() => zoomLevel++}
      class="h-12 w-12 transition-transform active:scale-90 disabled:opacity-40"
    >
      <img
        src={zoomInIcon}
        alt=""
        width="48"
        height="48"
        draggable="false"
        class="h-full w-full [image-rendering:pixelated]"
      />
    </button>
    <button
      type="button"
      aria-label="Zoom out"
      disabled={zoomLevel <= 0}
      onclick={() => zoomLevel--}
      class="h-12 w-12 transition-transform active:scale-90 disabled:opacity-40"
    >
      <img
        src={zoomOutIcon}
        alt=""
        width="48"
        height="48"
        draggable="false"
        class="h-full w-full [image-rendering:pixelated]"
      />
    </button>
    <button
      type="button"
      aria-label="Open spell book"
      aria-haspopup="dialog"
      onclick={() => (bookOpen = true)}
      class="h-16 w-16 transition-transform active:scale-90"
    >
      <img
        src={bookIcon}
        alt=""
        width="48"
        height="48"
        draggable="false"
        class="h-full w-full [image-rendering:pixelated]"
      />
    </button>
  </div>
</div>

<SpellBook bind:open={bookOpen} shapes={game.shapes} {casts} {selected} />

<!-- The controls, hidden while the map takes the whole screen; they still
     run (stream, state), ready for the UI to be built on top of the map. -->
<div hidden>
  <p>
    <a href="/">home</a> · <a href="/team">team</a> · {user?.name} ·
    {connected ? "live ●" : "reconnecting…"}
  </p>

  {#if team && team.status !== "IN_GAME"}
    <p>Your team hasn't started yet. <a href="/team">Back to the lobby</a>.</p>
  {/if}

  <h1>Play</h1>

  <p>
    Health: <progress max={game.health} value={ts.health}></progress>
    {ts.health}/{game.health}
  </p>
  {#if ts.knocked_out}
    <p style="color: red">
      <b>Knocked out.</b> Your team can't select or scan until an organiser revives
      it.
    </p>
  {/if}

  <h2>Boss {ts.bosses_defeated + (boss ? 1 : 0)}/{bosses.length}</h2>
  {#if boss && ts.boss}
    <p>
      <img
        src="/{boss.asset}"
        alt=""
        width="96"
        height="96"
        onerror={(e) => ((e.currentTarget as HTMLImageElement).hidden = true)}
      />
      <b>{boss.name}</b>: {boss.description}
    </p>
    <p>
      <progress max={boss.hp} value={boss.hp - ts.boss.damage}></progress>
      {boss.hp - ts.boss.damage} / {boss.hp} HP
      {#if ts.damage_multiplier !== 1}· damage ×{ts.damage_multiplier}{/if}
    </p>
    {#if ts.boss.next_attack_at}
      <p>
        Attacks every {Math.round((boss.attack_interval_s / 60) * 10) / 10} min for
        {boss.attack_damage}: next in <b>{countdown(ts.boss.next_attack_at)}</b>
      </p>
    {/if}
  {:else if bosses.length > 0}
    <p><b>Every boss defeated!</b> Casts still count toward your patterns.</p>
  {:else}
    <p>No bosses loaded.</p>
  {/if}

  <h2>1. Pick a type</h2>
  {#if !isLeader}
    <p><small>Your leader picks the type; you scan.</small></p>
  {/if}
  {#if ts.unlocked_points < game.max_pattern_points}
    <p>
      <small>
        Patterns of {game.locked_pattern_points}+ points unlock after defeating
        {game.unlock_after_bosses} bosses ({ts.bosses_defeated}/{game.unlock_after_bosses}).
      </small>
    </p>
  {/if}
  <p>
    {#each game.shapes as s (s.id)}
      <button
        disabled={busy ||
          ts.knocked_out ||
          !isLeader ||
          ts.locked_shapes.includes(s.id)}
        onclick={() => selectShape(s.id)}
        style:font-weight={s.id === selected ? "bold" : "normal"}
      >
        {ts.locked_shapes.includes(s.id) ? "🔒 " : ""}{s.name} ({s.points.join("/")} pts · {s.damage.min}–{s.damage.max} dmg ·
        {s.patterns} patterns){ts.shapes.includes(s.id) ? " ✓" : ""}
      </button>
    {/each}
  </p>
  {#if selected}
    <p>
      Selected: <b>{selectedShape?.name ?? selected}</b>
      <button disabled={busy || !isLeader} onclick={restart}
        >restart attempt</button
      >
      <button disabled={busy || !isLeader} onclick={giveUp}>give up</button>
    </p>
    <p>
      <small>
        No time limit, and anyone can scan any of the points. A point that
        doesn't fit any
        {selectedShape?.name ?? selected} pattern spoils the attempt: restart to
        clear it.
      </small>
    </p>
  {:else}
    <p>No type selected.</p>
  {/if}

  <h2>2. Enter waypoints</h2>
  {#if ts.poisoned}
    <p style="color: red">
      A scan doesn't match any {selectedShape?.name ?? selected} pattern, so this
      attempt can't cast.
      <button disabled={busy || !isLeader} onclick={restart}
        >restart attempt</button
      >
    </p>
  {/if}
  <form onsubmit={scan}>
    <input
      bind:value={waypoint}
      inputmode="numeric"
      pattern="[0-9]{4}"
      maxlength="4"
      placeholder="1001"
      required
      disabled={!selected || ts.poisoned || ts.knocked_out || busy}
    />
    <button disabled={!selected || ts.poisoned || ts.knocked_out || busy}
      >Scan</button
    >
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
          {s.waypoint_id} by {memberName(s.user_id)}{s.user_id === user?.id
            ? " (you)"
            : ""}, {ago(s.scanned_at)}{s.wrong ? " ✗ doesn't fit" : ""}
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
          <b>{c.shape}</b>
          {c.pattern.join("-")} ({c.damage} dmg{c.boss !== null
            ? `, ${c.dealt} to ${bossName(c.boss)}`
            : ""}{c.defeated_boss ? ", defeated it" : ""}) by {memberName(
            c.cast_by,
          )},
          {new Date(c.cast_at).toLocaleTimeString()}{c.already_completed
            ? " (cast before)"
            : ""}
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
</div>
