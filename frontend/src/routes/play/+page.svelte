<script lang="ts">
  import MapView, { ZOOM_LEVELS } from "#lib/components/MapView.svelte";
  import SpellBook from "#lib/components/SpellBook.svelte";
  import DiamondBackdrop from "#lib/components/DiamondBackdrop.svelte";
  import { MAP_POINTS } from "#lib/game/mapPoints.ts";
  import { testCasts, testTeamState } from "#lib/game/testTeamState.ts";
  import qrSprite from "#lib/assets/UserInterfaceButtons/qr-sprite.webp";
  import zoomInIcon from "#lib/assets/UserInterfaceButtons/zoom-in-icon.webp";
  import zoomOutIcon from "#lib/assets/UserInterfaceButtons/zoom-out-icon.webp";
  import bookIcon from "#lib/assets/UserInterfaceButtons/book-icon.webp";
  import scottFace from "#lib/assets/icons/scott-face.png";
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
  import jsQR from "jsqr";
  import { onMount, tick, untrack } from "svelte";
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

  // TEST MODE (ForRohitJG.MD): everything on /play reads testTeamState and
  // testCasts, and selecting, clearing and scanning change a local copy
  // instead of calling the backend. false = the live team state (ts) and
  // cast log, which keep updating underneath either way.
  const TEST_MODE = true;
  let testView = $state(structuredClone(testTeamState));
  let view = $derived(TEST_MODE ? testView : ts);
  let shownCasts = $derived(TEST_MODE ? testCasts : casts);
  // The waypoints scanned in the current attempt, whose markers wiggle.
  let markedWaypoints = $derived(view.stack.map((s) => s.waypoint_id));

  // The boss being fought, from the static list by the state's serial, or
  // null once every boss is defeated (or none are loaded).
  let boss = $derived(
    view.boss ? (bosses.find((b) => b.serial === view.boss!.serial) ?? null) : null,
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

  let selected = $derived(view.selection?.shape ?? null);
  // Only the leader picks a type, restarts or gives up; everyone scans.
  let isLeader = $derived(!!team && team.leader_id === user?.id);
  let selectedShape = $derived(game.shapes.find((s) => s.id === selected));
  let busy = $state(false);
  let message = $state<{ ok: boolean; text: string } | null>(null);

  onMount(() => {
    const ticker = setInterval(() => (now = clock.now()), 1000);
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
      // and the boss's attacks. Not in test mode: they're the real team's.
      if (!fresh || TEST_MODE) return;
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
      clearInterval(ticker);
      events.close();
      closeScanner();
    };
  });

  // What a player sees for a refused scan or selection; other codes show the
  // backend's own code and message.
  const errorText: Record<string, string> = {
    invalid_qr: "That's not a Labyrinth code.",
    qr_expired: "The code rotated: scan it again.",
    waypoint_on_cooldown:
      "That point was just used in a cast. It opens again once its QR cooldown ends.",
    shape_on_cooldown: "You can't cast the same class twice in a row.",
    network_error: "No connection: check your signal and try again.",
  };
  const refused = (json: { code?: string; message?: string }) => ({
    ok: false,
    text: (json.code && errorText[json.code]) ?? `${json.code}: ${json.message}`,
  });

  // Never throws: a network error (the phone lost signal) comes back as a
  // refusal, network_error, so every caller still clears busy.
  const call = async (path: string, method: string, body?: unknown) => {
    try {
      const res = await fetch(path, {
        method,
        headers: { "Content-Type": "application/json" },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      return { res, json: await res.json().catch(() => ({})) };
    } catch {
      return {
        res: new Response(null, { status: 503 }),
        json: { code: "network_error", message: "network error" },
      };
    }
  };

  // TEST MODE: what the backend's select, clear and scan do to the state,
  // applied to testView instead (ForRohitJG.MD). No pattern data here, so a
  // scan is never wrong and never completes a spell.
  const testChange = (change: (s: TeamState) => void) => {
    change(testView);
    testView.version.seq++;
  };
  const testSelect = (shape: string, restart: boolean) => {
    if (view.knocked_out) return (message = refused({ code: "knocked_out", message: "your team is knocked out" }));
    if (view.locked_shapes.includes(shape))
      return (message = refused({ code: "shape_locked", message: "this shape's patterns are locked" }));
    if (view.cooldown_shapes.includes(shape)) return (message = refused({ code: "shape_on_cooldown" }));
    testChange((s) => {
      s.selection = { shape, selected_by: user?.id ?? "", selected_at: new Date(now).toISOString() };
      s.stack = [];
      s.poisoned = false;
    });
    message = restart
      ? { ok: true, text: "attempt restarted: every scan cleared (test)" }
      : { ok: true, text: `selected ${shape}: stack purged (test)` };
  };
  const testGiveUp = () => {
    testChange((s) => {
      s.selection = null;
      s.stack = [];
      s.poisoned = false;
    });
    message = { ok: true, text: "selection cleared (test)" };
  };
  const testScan = (qr: string) => {
    // A token is "<waypoint>:<unix seconds>.<signature>"; not verified here.
    const id = qr.split(":")[0];
    if (!MAP_POINTS.some((p) => p.waypoint === id)) return (message = refused({ code: "invalid_qr" }));
    if (!view.selection) return (message = refused({ code: "no_shape_selected", message: "select a shape before scanning" }));
    if (view.stack.some((s) => s.waypoint_id === id))
      return (message = refused({ code: "waypoint_already_scanned", message: "this waypoint is already scanned" }));
    if (view.qr_cooldowns.some((c) => c.waypoint_id === id && Date.parse(c.until) > now))
      return (message = refused({ code: "waypoint_on_cooldown" }));
    testChange((s) => {
      s.stack.push({ waypoint_id: id, user_id: user?.id ?? "", scanned_at: new Date(now).toISOString(), wrong: false });
      if (!s.waypoints.includes(id)) s.waypoints = [...s.waypoints, id].sort();
    });
    message = { ok: true, text: `scan added: ${id} (test)` };
  };

  const selectShape = async (shape: string, restart = false) => {
    if (TEST_MODE) return testSelect(shape, restart);
    busy = true;
    const { res, json } = await call("/play/selection", "PUT", { shape });
    busy = false;
    if (!res.ok) return (message = refused(json));
    observe(TeamStateSchema.parse(json.payload));
    message = restart
      ? { ok: true, text: "attempt restarted: every scan cleared" }
      : { ok: true, text: `selected ${shape}: stack purged` };
  };

  // Restarting is reselecting the same type: the backend purges every scan,
  // the wrong one included. Only the leader can do it.
  const restart = () => selected && selectShape(selected, true);

  const giveUp = async () => {
    if (TEST_MODE) return testGiveUp();
    busy = true;
    const { res, json } = await call("/play/selection", "DELETE");
    busy = false;
    if (!res.ok) return (message = refused(json));
    observe(TeamStateSchema.parse(json.payload));
    message = { ok: true, text: "selection cleared" };
  };

  // The QR scanner: the camera, read with jsQR every 250 ms (as /test-scan
  // does). A code is the waypoint's signed, rotating token; it's posted as
  // is, and the backend verifies it and resolves the waypoint.
  let scanning = $state(false);
  let video = $state<HTMLVideoElement>();
  let stream: MediaStream | null = null;
  let loop: ReturnType<typeof setInterval> | null = null;
  // The last token posted: the camera sees the same code on every frame
  // until it rotates, and it's posted once.
  let lastToken = "";

  const openScanner = async () => {
    message = null;
    lastToken = "";
    scanning = true;
    try {
      stream = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: "environment" },
      });
      await tick();
      if (!scanning || !video) return closeScanner();
      video.srcObject = stream;
      await video.play();
      loop = setInterval(decode, 250);
    } catch (e) {
      message = { ok: false, text: `Camera unavailable: ${e}` };
    }
  };

  const closeScanner = () => {
    scanning = false;
    if (loop) clearInterval(loop);
    loop = null;
    stream?.getTracks().forEach((t) => t.stop());
    stream = null;
  };

  const decode = () => {
    if (!video || busy) return;
    const w = video.videoWidth;
    const h = video.videoHeight;
    if (!w || !h) return;
    const c = document.createElement("canvas");
    c.width = w;
    c.height = h;
    const ctx = c.getContext("2d");
    if (!ctx) return;
    ctx.drawImage(video, 0, 0, w, h);
    const code = jsQR(ctx.getImageData(0, 0, w, h).data, w, h);
    if (code?.data && code.data !== lastToken) {
      lastToken = code.data;
      void scan(code.data);
    }
  };

  const scan = async (qr: string) => {
    if (TEST_MODE) return testScan(qr);
    busy = true;
    const { res, json } = await call("/play/scan", "POST", { qr });
    busy = false;
    // A scan lost to the network can be retried with the same code.
    if (json.code === "network_error") lastToken = "";
    if (!res.ok) return (message = refused(json));
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
              text: `${result.state.stack.at(-1)?.waypoint_id ?? "That point"} doesn't match any ${name} pattern with the points already scanned. Restart the attempt.`,
            }
          : { ok: true, text: "scan added" };
  };

  const memberName = (id: string) =>
    team?.members.find((m: Team["members"][number]) => m.id === id)?.name ??
    id.slice(0, 8);
</script>

<!-- Top bar: full width, the top 25% of the screen, on the Scott Pilgrim
     menu's diamond backdrop (like /team) with a black edge above the map. A
     row for the back button, then the current boss's image, name, health
     and next attack, from view (the test data for now; ts, so it moves on
     every teammate's team_state, once it's live). -->
<div
  class="fixed inset-x-0 top-0 isolate flex h-[25dvh] flex-col gap-2 border-b-4 border-black px-4 py-3 font-pixel text-[16px] leading-[24px] text-white"
>
  <DiamondBackdrop class="absolute inset-0 -z-10" />
  <!-- The top row: back to the team page (left), the team's health (right). -->
  <div class="flex shrink-0 items-center justify-between gap-4">
    <a
      href="/team"
      aria-label="Back to team"
      class="pixel-box bg-ink/80 px-2 py-1 text-white hover:bg-pink active:translate-y-[2px] active:bg-pink active:pixel-box-pressed"
    >
      &lt;
    </a>
    <p class="flex items-center gap-2 text-[8px] leading-[16px] uppercase">
      <!-- No visible label: the face says whose health it is. -->
      <span class="sr-only">Team health</span>
      <!-- A step below the face beside it (32px), so it doesn't crowd the bar. -->
      <span class="text-[18px] leading-[32px] text-outline">x{view.health}</span>
      <img
        src={scottFace}
        alt=""
        width="64"
        height="64"
        draggable="false"
        class="h-8 w-8 [image-rendering:pixelated]"
      />
    </p>
  </div>
  <div class="flex min-h-0 flex-1 items-center gap-4">
    {#if boss && view.boss}
      {@const hp = Math.max(0, boss.hp - view.boss.damage)}
      <!-- The boss's image (static/<asset>), as tall as its row; hidden if
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
      <!-- The name in the menu's logo style (cyan, outlined), the health
           bar filled with its highlight pink. -->
      <div class="flex min-w-0 flex-1 flex-col justify-center gap-2">
        <p class="truncate py-1 font-display text-[32px] leading-[1] text-cyan uppercase heading-outline">
          {boss.name}
        </p>
        <div
          role="progressbar"
          aria-label="{boss.name}'s health"
          aria-valuemin={0}
          aria-valuemax={boss.hp}
          aria-valuenow={hp}
          class="pixel-box mx-1 h-4 bg-ink"
        >
          <div
            class="h-full bg-pink transition-[width] duration-300"
            style:width="{(hp / boss.hp) * 100}%"
          ></div>
        </div>
        <!-- The countdown is on the server's clock (now); null while
             knocked out, when nothing attacks. -->
        <p class="text-[8px] leading-[16px] text-outline">
          {hp} / {boss.hp} HP{#if view.boss.next_attack_at}
            · Attack in {countdown(view.boss.next_attack_at)}{/if}
        </p>
      </div>
    {:else if bosses.length > 0}
      <p class="uppercase text-outline">Every boss defeated!</p>
    {:else}
      <p class="uppercase text-outline">No bosses loaded.</p>
    {/if}
  </div>
</div>

<!-- The map: the rest of the screen, below the bar. -->
<div class="fixed inset-x-0 top-[25dvh] bottom-0">
  <MapView
    bind:level={zoomLevel}
    markers={MAP_POINTS}
    active={markedWaypoints}
    cooldowns={view.qr_cooldowns}
    {now}
  />

  <!-- QR scanner button, bottom left: opens the camera. -->
  <button
    type="button"
    aria-label="Scan QR code"
    aria-haspopup="dialog"
    onclick={openScanner}
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
    <!-- While a spell is selected, the button shows it instead of the book
         (a placeholder until the spells have images). An old message would
         show in the book, so opening clears it, as opening the scanner does. -->
    <button
      type="button"
      aria-label="Open spell book"
      aria-haspopup="dialog"
      onclick={() => {
        message = null;
        bookOpen = true;
      }}
      class="h-16 w-16 transition-transform active:scale-90"
    >
      {#if selected}
        <span
          class="pixel-box flex h-full w-full items-center justify-center bg-paper p-1 text-center font-pixel text-[8px] leading-[12px] text-ink uppercase"
        >
          Image of the spell
        </span>
      {:else}
        <img
          src={bookIcon}
          alt=""
          width="48"
          height="48"
          draggable="false"
          class="h-full w-full [image-rendering:pixelated]"
        />
      {/if}
    </button>
  </div>
</div>

<!-- The QR scanner: the camera over the whole screen, the last result
     below it. It stays open so a player can scan several points in a row. -->
{#if scanning}
  <div
    role="dialog"
    aria-modal="true"
    aria-label="Scan QR code"
    class="fixed inset-0 z-30 flex flex-col gap-4 bg-ink p-4 font-pixel text-[16px] leading-[24px] text-white"
  >
    <header class="flex items-center justify-between">
      <h2 class="uppercase">Scan a point</h2>
      <button
        type="button"
        onclick={closeScanner}
        class="pixel-box bg-ink px-3 py-1 uppercase hover:bg-pink active:pixel-box-pressed"
      >
        Close
      </button>
    </header>
    <!-- svelte-ignore a11y_media_has_caption -->
    <video
      bind:this={video}
      autoplay
      playsinline
      muted
      class="min-h-0 w-full flex-1 object-cover"
    ></video>
    <p
      aria-live="polite"
      class="min-h-[48px] text-[8px] leading-[16px] uppercase"
      class:text-pink={message && !message.ok}
      class:text-cyan={message?.ok}
    >
      {busy ? "Checking…" : (message?.text ?? "Point the camera at the point's QR code.")}
    </p>
  </div>
{/if}

<SpellBook
  bind:open={bookOpen}
  shapes={game.shapes}
  casts={shownCasts}
  {selected}
  onselect={(s) => selectShape(s)}
  onclear={giveUp}
  {busy}
  {isLeader}
  canSelect={!view.knocked_out}
  locked={view.locked_shapes}
  cooldownShapes={view.cooldown_shapes}
  {message}
/>

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
    Health: <progress max={game.health} value={view.health}></progress>
    {view.health}/{game.health}
  </p>
  {#if view.knocked_out}
    <p style="color: red">
      <b>Knocked out.</b> Your team can't select or scan until an organiser revives
      it.
    </p>
  {/if}

  <h2>Boss {view.bosses_defeated + (boss ? 1 : 0)}/{bosses.length}</h2>
  {#if boss && view.boss}
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
      <progress max={boss.hp} value={boss.hp - view.boss.damage}></progress>
      {boss.hp - view.boss.damage} / {boss.hp} HP
      {#if view.damage_multiplier !== 1}· damage ×{view.damage_multiplier}{/if}
    </p>
    {#if view.boss.next_attack_at}
      <p>
        Attacks every {Math.round((boss.attack_interval_s / 60) * 10) / 10} min for
        {boss.attack_damage}: next in <b>{countdown(view.boss.next_attack_at)}</b>
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
  {#if view.unlocked_points < game.max_pattern_points}
    <p>
      <small>
        Patterns of {game.locked_pattern_points}+ points unlock after defeating
        {game.unlock_after_bosses} bosses ({view.bosses_defeated}/{game.unlock_after_bosses}).
      </small>
    </p>
  {/if}
  <p>
    {#each game.shapes as s (s.id)}
      <button
        disabled={busy ||
          view.knocked_out ||
          !isLeader ||
          view.locked_shapes.includes(s.id)}
        onclick={() => selectShape(s.id)}
        style:font-weight={s.id === selected ? "bold" : "normal"}
      >
        {view.locked_shapes.includes(s.id) ? "🔒 " : ""}{s.name} ({s.points.join("/")} pts · {s.damage.min}–{s.damage.max} dmg ·
        {s.patterns} patterns){view.shapes.includes(s.id) ? " ✓" : ""}
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

  <h2>2. Scan waypoints</h2>
  {#if view.poisoned}
    <p style="color: red">
      A scan doesn't match any {selectedShape?.name ?? selected} pattern, so this
      attempt can't cast.
      <button disabled={busy || !isLeader} onclick={restart}
        >restart attempt</button
      >
    </p>
  {/if}
  <p>
    <button
      onclick={openScanner}
      disabled={!selected || view.poisoned || view.knocked_out || busy}
      >Scan a QR code</button
    >
  </p>

  {#if message}
    <p style:color={message.ok ? "green" : "red"}>{message.text}</p>
  {/if}

  <h3>Stack (bottom → top)</h3>
  {#if view.stack.length === 0}
    <p>empty</p>
  {:else}
    <ol>
      {#each view.stack as s (s.waypoint_id)}
        <li style:color={s.wrong ? "red" : null}>
          {s.waypoint_id} by {memberName(s.user_id)}{s.user_id === user?.id
            ? " (you)"
            : ""}, {ago(s.scanned_at)}{s.wrong ? " ✗ doesn't fit" : ""}
        </li>
      {/each}
    </ol>
  {/if}

  <h2>Casts</h2>
  {#if shownCasts.length === 0}
    <p>none yet</p>
  {:else}
    <ol>
      {#each shownCasts as c (c.cast_at)}
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
  <p>Types completed: {view.shapes.length ? view.shapes.join(", ") : "none"}</p>
  <p>Patterns completed: {view.patterns.length}</p>
  <ul>
    {#each view.patterns as p (p.join("-"))}<li>{p.join("-")}</li>{/each}
  </ul>
  <p>Waypoints visited: {view.waypoints.join(", ") || "none"}</p>
</div>
