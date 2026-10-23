<script lang="ts">
  import MapView, { BURST_MS, ZOOM_LEVELS } from "#lib/components/MapView.svelte";
  import SpellFigure from "#lib/components/SpellFigure.svelte";
  import { spellFigure } from "#lib/game/spellFigure.ts";
  import SpellBook from "#lib/components/SpellBook.svelte";
  import DiamondBackdrop from "#lib/components/DiamondBackdrop.svelte";
  import { BANNERS } from "#lib/game/banners.ts";
  import BossSprite from "#lib/components/BossSprite.svelte";
  import fitText from "#lib/utils/fitText.ts";
  import { MAP_POINTS } from "#lib/game/mapPoints.ts";
  import bookIcon from "#lib/assets/UserInterfaceButtons/spellbook_64.png";
  import scottFace from "#lib/assets/icons/scott-face.png";
  import damageMultiplierIcon from "#lib/assets/powerups/damage_multiplier_64.png";
  import heartIcon from "#lib/assets/powerups/gain_a_life_64.png";
  import shortCooldownIcon from "#lib/assets/powerups/short_cooldown_64.png";
  import instantCastIcon from "#lib/assets/powerups/instant_cast_64.png";
  import { ServerClock } from "#lib/game/serverClock.ts";
  import {
    CastLogSchema,
    QTESchema,
    ScanResultSchema,
    TeamStateSchema,
    type Boss,
    type Cast,
    type QTE,
    type TeamState,
  } from "#lib/types/Game.type.ts";
  import { envelope, isNewer } from "#lib/types/Live.type.ts";
  import { TeamSchema, type Team } from "#lib/types/Team.type.ts";
  import { z } from "zod";
  import { afterNavigate, goto } from "$app/navigation";
  import jsQR from "jsqr";
  import { onMount, tick, untrack } from "svelte";
  import { fade } from "svelte/transition";
  import type { PageProps } from "./$types";

  let { data }: PageProps = $props();

  // The map's zoom step (an index into ZOOM_LEVELS), moved by the zoom
  // buttons. It opens at 1.25×.
  let zoomLevel = $state(ZOOM_LEVELS.indexOf(1.25));
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

  // A spell just cast, played on the map before the state shows it: its
  // points held, glowing and joined, for CAST_HOLD_MS; then each bursts
  // once (BURST_MS). Only then is the state with the cast shown (held
  // meanwhile, with any newer one), so the boss takes the damage and the
  // points turn to cooled-down coins after the burst.
  const CAST_HOLD_MS = 1000;
  let castFx = $state<{ points: string[]; burst: boolean } | null>(null);
  let held: TeamState | null = null;
  let castTimer: ReturnType<typeof setTimeout> | undefined;
  const playCast = (points: string[]) => {
    castFx = { points, burst: false };
    castTimer = setTimeout(() => {
      castFx = { points, burst: true };
      castTimer = setTimeout(() => {
        castFx = null;
        if (held && isNewer(ts.version, held.version)) live = held;
        held = null;
      }, BURST_MS);
    }, CAST_HOLD_MS);
  };
  // The newest state seen, shown or held.
  const latest = () => held ?? ts;

  // Every copy of the state, from the stream or a response, goes through
  // here: it's kept only if newer than the newest seen (by version; they
  // can arrive in either order). One carrying a new cast, made in the last
  // 10 s (not one missed while away), is held while the cast plays.
  const observe = (s: TeamState) => {
    if (!isNewer(latest().version, s.version)) return;
    const lc = s.last_cast;
    if (lc && !casts.some((c) => c.cast_at === lc.cast_at))
      casts = [lc, ...casts];
    if (s.cast_count > casts.length) refetchCasts();
    const newCast =
      lc &&
      lc.cast_at !== latest().last_cast?.cast_at &&
      Date.parse(lc.cast_at) > clock.now() - 10_000;
    if (castFx || newCast) {
      held = s;
      if (!castFx && lc) playCast(lc.pattern);
      return;
    }
    live = s;
  };

  // The waypoints scanned in the current attempt, whose markers glow.
  let markedWaypoints = $derived(ts.stack.map((s) => s.waypoint_id));

  // The boss being fought, from the static list by the state's serial, or
  // null once every boss is defeated (or none are loaded).
  let boss = $derived(
    ts.boss ? (bosses.find((b) => b.serial === ts.boss!.serial) ?? null) : null,
  );
  // The boss's reactions on the top bar, each played once by its sprite,
  // which clears it when done. A hit (its damage rising) plays the damage
  // animation; hits counts them, so another hit mid-way restarts it. A
  // killing hit keeps the defeated boss (dying) for its damage, then its
  // death, then the next boss shows; that one's hits meanwhile play
  // nothing. Only changes after the page loads start any.
  let dying = $state<Boss | null>(null);
  let hurt = $state(false);
  let hits = $state(0);
  let lastDefeated = untrack(() => ts.bosses_defeated);
  let lastBoss = untrack(() => boss);
  let lastDamage = untrack(() => ts.boss?.damage ?? 0);
  $effect(() => {
    const defeated = ts.bosses_defeated;
    const current = boss;
    const damage = ts.boss?.damage ?? 0;
    // Only the state above reruns this, not the reactions it sets.
    untrack(() => {
      const killed = defeated > lastDefeated && lastBoss;
      if (killed) dying = lastBoss;
      if (
        killed ||
        (!dying && current && current.serial === lastBoss?.serial && damage > lastDamage)
      ) {
        hurt = true;
        hits++;
      }
    });
    lastDefeated = defeated;
    lastBoss = current;
    lastDamage = damage;
  });
  let shown = $derived(dying ?? boss);

  // Counts the team's health drops since the page loaded; each one
  // restarts the health's blink on the top bar (keyed on it) and shows the
  // attack over the map until the player closes it: the boss (only an
  // attack lowers health), what it took, and the health left.
  let healthDrops = $state(0);
  let lastHealth = untrack(() => ts.health);
  let attack = $state<{ boss: Boss; dealt: number; health: number } | null>(null);
  $effect(() => {
    const health = ts.health;
    if (health < lastHealth)
      untrack(() => {
        healthDrops++;
        if (!boss) return;
        attack = { boss, dealt: lastHealth - health, health };
      });
    lastHealth = health;
  });
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
  let message = $state<{ ok: boolean; text: string } | null>(null);
  // A short notice over the map, gone after 5 s (e.g. the spell just picked).
  let toast = $state<{ ok: boolean; text: string } | null>(null);
  let toastTimer: ReturnType<typeof setTimeout> | undefined;
  const showToast = (text: string, ok = true) => {
    clearTimeout(toastTimer);
    toast = { ok, text };
    toastTimer = setTimeout(() => (toast = null), 5000);
  };

  // A hint over the map for 2 s, when the player comes in from /team (not
  // on a reload or a direct link).
  let dragHint = $state(false);
  afterNavigate(({ from }) => {
    if (from?.url.pathname !== "/team") return;
    dragHint = true;
    setTimeout(() => (dragHint = false), 2000);
  });

  // Powerups' names, by kind (the backend's wire names).
  const POWERUPS: Record<string, string> = {
    damage_multiplier: "Damage boost",
    heart: "Heart",
    short_qr_cooldown: "Quick cooldown",
    instant_cast: "Instant cast",
  };
  const powerupName = (kind: string) => POWERUPS[kind] ?? kind;
  // What each does, under 15 words, for its card. The numbers are
  // config.toml's [powerups] (×2, +1, 2 min for 5 min): update them with it.
  const POWERUP_DESCRIPTIONS: Record<string, string> = {
    damage_multiplier: "Your team's next spell deals double damage.",
    heart: "Restores one health to your team.",
    short_qr_cooldown: "For 5 minutes, cast points reopen after 2 minutes, not 5.",
    instant_cast: "Tap any points on the map to cast them at once, ignoring cooldowns.",
  };
  // Their icons, by kind, for the map.
  const POWERUP_ICONS: Record<string, string> = {
    damage_multiplier: damageMultiplierIcon,
    heart: heartIcon,
    short_qr_cooldown: shortCooldownIcon,
    instant_cast: instantCastIcon,
  };

  // The quick-time event: the open quiz, or the one just closed with its
  // answer, from the game-wide qte event; null when there's none. hiddenQTE
  // is the one the player closed, so it stays closed until the next.
  let qte = $state<QTE | null>(null);
  let hiddenQTE = $state<number | null>(null);
  // The team's answer to it, if any (any member answers, once per team).
  let qteAnswer = $derived(qte && ts.qte?.qte_id === qte.id ? ts.qte : null);

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
      const fresh = isNewer(latest().version, next.version);
      const heldBefore = ts.powerups.held;
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
      } else if (type === "powerup_granted") {
        const got = next.powerups.held.find((k) => !heldBefore.includes(k));
        if (got) showToast(`Powerup: ${powerupName(got)}!`);
      } else if (type === "powerup_ended") {
        showToast("Quick cooldown ended");
      }
    });
    events.addEventListener("qte", (e) => {
      qte = envelope(QTESchema.nullable()).parse(JSON.parse(e.data)).message;
    });
    // /play needs a team: without one (never had one, or it's gone) the
    // player belongs on /team, where they can create or join one.
    events.addEventListener("no_team", () => {
      events.close();
      goto("/team");
    });
    return () => {
      clearInterval(ticker);
      clearTimeout(toastTimer);
      clearTimeout(scanPopupTimer);
      clearTimeout(castTimer);
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
    waypoint_already_scanned: "That point is already scanned in this attempt.",
    no_shape_selected: "Your leader hasn't picked a spell yet.",
    selection_poisoned: "A wrong point was scanned: restart the attempt.",
    not_in_game: "Your team isn't in the game.",
    shape_on_cooldown: "You can't cast the same class twice in a row.",
    network_error: "No connection: check your signal and try again.",
    knocked_out: "Knocked out: an organiser must revive your team.",
    not_leader: "Only the team leader can do that.",
    powerup_not_held: "Your team doesn't have that powerup.",
    powerup_active: "That powerup is already running.",
    full_health: "Already at full health.",
    unknown_pattern: "Those points don't make a spell.",
    pattern_locked: "That spell is locked until your team defeats more bosses.",
    qte_closed: "Too late: the quiz has closed.",
    qte_already_answered: "Your team has already answered.",
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

  const selectShape = async (shape: string, restart = false) => {
    busy = true;
    const { res, json } = await call("/play/selection", "PUT", { shape });
    busy = false;
    if (!res.ok) return (message = refused(json));
    observe(TeamStateSchema.parse(json.payload));
    if (restart) {
      message = { ok: true, text: "attempt restarted: every scan cleared" };
      return;
    }
    // Picked from the book: close it and say what was picked.
    bookOpen = false;
    showToast(`${game.shapes.find((s) => s.id === shape)?.name ?? shape} selected`);
  };

  // Restarting is reselecting the same type: the backend purges every scan,
  // the wrong one included. Only the leader can do it.
  const restart = () => selected && selectShape(selected, true);

  const giveUp = async () => {
    busy = true;
    const { res, json } = await call("/play/selection", "DELETE");
    busy = false;
    if (!res.ok) return (message = refused(json));
    observe(TeamStateSchema.parse(json.payload));
    message = { ok: true, text: "selection cleared" };
  };

  // Powerups: everyone sees them, and tapping one opens a card saying what
  // it does (powerupCard, its kind); only the leader can use it from there,
  // so one isn't wasted by a stray tap. Instant cast then goes on to
  // picking its points on the map.
  let powerupCard = $state<string | null>(null);
  const useFromCard = () => {
    const kind = powerupCard;
    if (!kind || busy) return;
    powerupCard = null;
    if (kind === "instant_cast") {
      picked = [];
      picking = true;
      return;
    }
    void usePowerup(kind);
  };
  const usePowerup = async (kind: string, body?: unknown) => {
    busy = true;
    const { res, json } = await call(`/play/powerups/${kind}`, "POST", body ?? {});
    busy = false;
    if (!res.ok) return showToast(refused(json).text, false);
    const next = TeamStateSchema.parse(json.payload);
    observe(next);
    const lc = next.last_cast;
    showToast(
      kind === "instant_cast" && lc
        ? `Instant cast: ${lc.dealt} dmg${lc.boss !== null ? ` to ${bossName(lc.boss)}` : ""}${lc.defeated_boss ? ", defeated!" : ""}`
        : `${powerupName(kind)} used`,
    );
  };

  // Instant cast: the leader taps the pattern's points on the map (they
  // wiggle and are outlined as picked), then casts. Cooldowns don't apply,
  // so any point can be picked; at most the team's unlocked_points.
  let picking = $state(false);
  let picked = $state<string[]>([]);
  const pick = (id: string) => {
    if (picked.includes(id)) picked = picked.filter((p) => p !== id);
    else if (picked.length < ts.unlocked_points) picked = [...picked, id];
  };
  const instantCast = async () => {
    await usePowerup("instant_cast", { pattern: picked });
    if (!ts.powerups.held.includes("instant_cast")) picking = false;
  };

  // Answering the quiz, for the team.
  const answerQTE = async (option: number) => {
    if (!qte) return;
    busy = true;
    const { res, json } = await call(`/play/qte/${qte.id}`, "POST", { option });
    busy = false;
    if (!res.ok) return showToast(refused(json).text, false);
    observe(TeamStateSchema.parse(json.payload));
  };

  // The scanner button's QR code, drawn on a 13×13 grid (# is ink): three
  // finder squares and some data. One path of a rect per run of #s.
  const QR_GLYPH = [
    "#####...#####",
    "#...#.#.#...#",
    "#.#.#...#.#.#",
    "#...#.#.#...#",
    "#####...#####",
    ".............",
    ".#.#..#.#.#..",
    ".............",
    "#####...#.#.#",
    "#...#.#..#.#.",
    "#.#.#...#.#..",
    "#...#.#.....#",
    "#####....#.##",
  ]
    .flatMap((row, y) =>
      [...row.matchAll(/#+/g)].map((m) => `M${m.index} ${y}h${m[0].length}v1h-${m[0].length}z`),
    )
    .join("");

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

  // Every scan's result, popped up over the camera, so it's seen without
  // looking down at the line under it: a refusal for 2 s, a scan that
  // counted until it's tapped.
  let scanPopup = $state<{ ok: boolean; title: string; text: string } | null>(null);
  let scanPopupTimer: ReturnType<typeof setTimeout> | undefined;
  const popScan = (ok: boolean, title: string, text: string) => {
    clearTimeout(scanPopupTimer);
    scanPopup = { ok, title, text };
    if (!ok) scanPopupTimer = setTimeout(() => (scanPopup = null), 2000);
  };

  const scan = async (qr: string) => {
    busy = true;
    const { res, json } = await call("/play/scan", "POST", { qr });
    busy = false;
    // A scan lost to the network can be retried with the same code.
    if (json.code === "network_error") lastToken = "";
    if (!res.ok) {
      message = refused(json);
      return popScan(false, "Scan failed", message.text);
    }
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
    if (result.outcome === "wrong_scan") popScan(false, "Wrong point", message.text);
    else
      popScan(
        true,
        "QR scanned",
        result.outcome === "pattern_completed"
          ? `Spell cast: ${message.text}. You can close the scanner.`
          : "You can close the scanner.",
      );
  };

  const memberName = (id: string) =>
    team?.members.find((m: Team["members"][number]) => m.id === id)?.name ??
    id.slice(0, 8);
</script>

<!-- Top bar: full width, the top 25% of the screen, on the Scott Pilgrim
     menu's diamond backdrop (like /team) with a black edge above the map. A
     row for the back button, then the current boss's image, name, health
     and next attack, from ts, so it moves on every
     teammate's team_state. -->
<div
  class="fixed inset-x-0 top-0 isolate flex h-[25dvh] flex-col gap-2 overflow-hidden border-b-4 border-black px-4 py-3 font-pixel text-[16px] leading-[24px] text-white"
>
  <!-- In the shown boss's colours; the menu's blues once all are beaten. -->
  <DiamondBackdrop colors={shown ? BANNERS[shown.serial] : undefined} class="absolute inset-0 -z-10" />
  <!-- The top row: back to the team page (left), the team's health (right). -->
  <div class="flex shrink-0 items-center justify-between gap-4">
    <a
      href="/team"
      aria-label="Back to team"
      class="pixel-box bg-ink/80 px-2 py-1 text-white hover:bg-pink active:translate-y-[2px] active:bg-pink active:pixel-box-pressed"
    >
      &lt;
    </a>
    <!-- Blinks hard three times when the health drops. -->
    {#key healthDrops}
      <p
        class="flex items-center gap-2 text-[8px] leading-[16px] uppercase"
        class:animate-blink-thrice={healthDrops > 0}
      >
        <!-- No visible label: the face says whose health it is. -->
        <span class="sr-only">Team health</span>
        <!-- A step below the face beside it (32px), so it doesn't crowd the bar. -->
        <span class="text-[18px] leading-[32px] text-outline">x{ts.health}</span>
        <img
          src={scottFace}
          alt=""
          width="64"
          height="64"
          draggable="false"
          class="h-8 w-8 [image-rendering:pixelated]"
        />
      </p>
    {/key}
  </div>
  <div class="flex min-h-0 flex-1 items-center gap-4">
    {#if shown}
      {@const hp = dying ? 0 : Math.max(0, shown.hp - (ts.boss?.damage ?? 0))}
      <!-- The boss, as tall as its row: animated if it has a spritesheet,
           else its image (static/<asset>), else nothing, leaving the name
           and health. A hit plays its damage; a defeated boss its damage
           then its death before the next shows. Keyed by serial so the next
           boss never inherits the last one's sheet or hiding. -->
      {#key shown.serial}
        <BossSprite
          serial={shown.serial}
          fallback={shown.asset}
          animation={hurt ? "damage" : dying ? "death" : "idle"}
          replay={hits}
          onend={() => {
            if (hurt) hurt = false;
            else dying = null;
          }}
          class="h-full w-auto shrink-0"
        />
      {/key}
      <!-- The name in the menu's logo style (cyan, outlined), shrunk from
           32px until all of it fits the bar; the health bar filled with
           its highlight pink, the HP left written across it. -->
      <div class="flex min-w-0 flex-1 flex-col justify-center gap-2">
        <p
          {@attach fitText({ max: 32, min: 12 })}
          class="truncate py-1 font-display text-[32px] leading-[1] text-cyan uppercase heading-outline"
        >
          {shown.name}
        </p>
        <!-- A hit slides the pink down smoothly, and a white trail behind it
             follows a moment later, showing what the hit took. Keyed by
             boss, so the next one's full bar appears at once, not slid up
             from the last one's empty one. -->
        {#key shown.serial}
          <div
            role="progressbar"
            aria-label="{shown.name}'s health"
            aria-valuemin={0}
            aria-valuemax={shown.hp}
            aria-valuenow={hp}
            class="pixel-box relative mx-1 h-4 bg-ink"
          >
            <div
              class="absolute inset-y-0 left-0 bg-white transition-[width] delay-300 duration-700 ease-in-out"
              style:width="{(hp / shown.hp) * 100}%"
            ></div>
            <div
              class="absolute inset-y-0 left-0 bg-pink transition-[width] duration-500 ease-out"
              style:width="{(hp / shown.hp) * 100}%"
            ></div>
            <span
              class="absolute inset-0 flex items-center justify-center text-[8px] leading-[16px] text-outline"
            >
              {hp} HP
            </span>
          </div>
        {/key}
      </div>
      <!-- The next attack, in the bar's bottom right corner, on the
           server's clock (now): green with a whole interval to go,
           shading through to red as it runs out. Null while knocked out,
           when nothing attacks, and not shown while dying (ts.boss is
           already the next one). -->
      {#if !dying && ts.boss?.next_attack_at}
        {@const left = (Date.parse(ts.boss.next_attack_at) - now) / 1000}
        {@const share = Math.min(1, Math.max(0, left / shown.attack_interval_s))}
        <p
          class="pixel-box absolute right-0 bottom-0 px-2 text-[8px] leading-[16px] text-white text-outline"
          style:background-color="color-mix(in oklch, var(--color-go) {Math.round(share * 100)}%, var(--color-danger))"
        >
          Next Attack in {countdown(ts.boss.next_attack_at)}
        </p>
      {/if}
    {:else if bosses.length > 0}
      <p class="uppercase text-outline">Every boss defeated!</p>
    {:else}
      <p class="uppercase text-outline">No bosses loaded.</p>
    {/if}
  </div>
</div>

<!-- A powerup's icon (48px unless size says), or its name for a kind
     without one. -->
{#snippet powerupIcon(kind: string, size = "h-12 w-12")}
  {#if POWERUP_ICONS[kind]}
    <img
      src={POWERUP_ICONS[kind]}
      alt=""
      width="64"
      height="64"
      draggable="false"
      class="{size} [image-rendering:pixelated]"
    />
  {:else}
    <span class="pixel-box bg-gold-light px-2 py-1 text-ink">{powerupName(kind)}</span>
  {/if}
{/snippet}

<!-- The map: the rest of the screen, below the bar. -->
<div class="fixed inset-x-0 top-[25dvh] bottom-0">
  <MapView
    bind:level={zoomLevel}
    markers={MAP_POINTS}
    active={castFx ? castFx.points : picking ? picked : markedWaypoints}
    burst={castFx?.burst ? castFx.points : []}
    wrong={castFx || picking ? [] : ts.stack.filter((s) => s.wrong).map((s) => s.waypoint_id)}
    onpick={picking ? pick : undefined}
    cooldowns={ts.qr_cooldowns}
    {now}
  />

  <!-- The drag hint: a see-through box in the middle of the map, a pixel
       arrow off each side. Untouchable, so the drag it asks for works. -->
  {#if dragHint}
    <div
      out:fade={{ duration: 200 }}
      class="pointer-events-none absolute top-1/2 left-1/2 z-20 -translate-x-1/2 -translate-y-1/2"
    >
      <p class="pixel-box w-40 bg-black/60 px-3 py-2 text-center font-pixel text-[8px] leading-[16px] text-white uppercase">
        Drag to move the map around
      </p>
      {#each [["bottom-full left-1/2 mb-2 -translate-x-1/2", 0], ["top-1/2 left-full ml-2 -translate-y-1/2", 90], ["top-full left-1/2 mt-2 -translate-x-1/2", 180], ["top-1/2 right-full mr-2 -translate-y-1/2", 270]] as const as [place, turn] (turn)}
        <svg
          viewBox="0 0 8 8"
          shape-rendering="crispEdges"
          aria-hidden="true"
          class="absolute h-4 w-4 {place}"
          style:rotate="{turn}deg"
        >
          <path d="M3 0h2v1h1v1h1v1h-2v4h-2v-4h-2v-1h1v-1h1z" fill="white" stroke="var(--color-ink)" stroke-width="0.5" paint-order="stroke" />
        </svg>
      {/each}
    </div>
  {/if}

  <!-- The top of the map, where only its contents catch taps, so the map
       still pans around them: the powerups in the top left. -->
  <div class="pointer-events-none absolute inset-x-4 top-4 z-10 flex flex-col gap-2 font-pixel text-[8px] leading-[16px] uppercase">
    <!-- Powerups, top left, in a row of icons (named for screen readers),
         wrapping if it runs out of room: the ones running, each with its
         time left right below it, then the ones held, each opening its
         card. -->
    <div class="flex flex-wrap items-start gap-3 *:pointer-events-auto">
      {#each ts.powerups.active as a (a.kind)}
        <span class="flex flex-col items-center gap-2">
          {@render powerupIcon(a.kind)}
          <span class="pixel-box bg-cyan px-1 text-ink">
            <span class="sr-only">{powerupName(a.kind)}:</span>
            {a.until ? countdown(a.until) : "next spell"}
          </span>
        </span>
      {/each}
      {#each ts.powerups.held as kind (kind)}
        <button
          type="button"
          aria-label={powerupName(kind)}
          aria-haspopup="dialog"
          onclick={() => (powerupCard = kind)}
          class="transition-transform active:scale-90"
        >
          {@render powerupIcon(kind)}
        </button>
      {/each}
    </div>
  </div>

  <!-- The live quiz, if hidden, reopens from a small "?" in the top right,
       half a powerup icon's width. Until the team answers, its outline
       blinks hard, black to white; once answered, the whole button blinks
       between invisible and 75%, calling less with nothing left to do. -->
  {#if qte && qte.status === "open" && hiddenQTE === qte.id}
    <button
      type="button"
      aria-label="Open quiz{qteAnswer ? ' (answered)' : ''}, {countdown(qte.ends_at)} left"
      onclick={() => (hiddenQTE = null)}
      style:--flash-max={qteAnswer ? 0.75 : undefined}
      class="pixel-box absolute top-4 right-4 z-10 flex h-6 w-6 items-center justify-center bg-pink font-pixel text-[16px] leading-[16px] text-white active:pixel-box-pressed {qteAnswer
        ? 'animate-flash'
        : 'animate-edge-blink'}"
    >
      ?
    </button>
  {/if}

  <!-- Instant cast: while the leader taps the spell's points on the map,
       Cast (once at least 2 are picked) centred at the bottom of the map,
       between the scanner and the map controls, with X (stop, keeping the
       powerup) hung off its right side, so X never pushes Cast off centre. -->
  {#if picking}
    <div class="absolute bottom-6 left-1/2 z-10 -translate-x-1/2 font-pixel text-[8px] leading-[16px] uppercase">
      <button
        type="button"
        aria-label="Cast the {picked.length} picked points"
        disabled={busy || picked.length < 2}
        onclick={instantCast}
        class="pixel-box h-8 bg-gold px-3 text-ink active:pixel-box-pressed disabled:opacity-40"
      >
        {busy ? "…" : "Cast"}
      </button>
      <button
        type="button"
        aria-label="Cancel instant cast"
        onclick={() => (picking = false)}
        class="pixel-box absolute top-0 left-full ml-3 h-8 w-8 bg-panel text-paper active:pixel-box-pressed"
      >
        X
      </button>
    </div>
  {/if}

  <!-- QR scanner button, bottom left: opens the camera. A pixel box with
       a drawn QR code. -->
  <button
    type="button"
    aria-label="Scan QR code"
    aria-haspopup="dialog"
    onclick={openScanner}
    class="pixel-box absolute bottom-4 left-4 flex h-16 w-16 items-center justify-center bg-paper active:translate-y-[2px] active:pixel-box-pressed"
  >
    <svg viewBox="0 0 13 13" shape-rendering="crispEdges" aria-hidden="true" class="h-10 w-10">
      <path d={QR_GLYPH} fill="var(--color-ink)" />
    </svg>
  </button>

  <!-- Map controls, bottom right, in a column. Zoom steps through
         ZOOM_LEVELS, disabled at either end: small pixel boxes with a
         + or − drawn on an 8×8 grid. -->
  <div class="absolute right-4 bottom-4 flex flex-col items-center gap-3">
    <button
      type="button"
      aria-label="Zoom in"
      disabled={zoomLevel >= ZOOM_LEVELS.length - 1}
      onclick={() => zoomLevel++}
      class="pixel-box flex h-8 w-8 items-center justify-center bg-paper active:translate-y-[2px] active:pixel-box-pressed disabled:opacity-40"
    >
      <svg viewBox="0 0 8 8" shape-rendering="crispEdges" aria-hidden="true" class="h-4 w-4">
        <path d="M3 1h2v2h2v2h-2v2h-2v-2h-2v-2h2z" fill="var(--color-ink)" />
      </svg>
    </button>
    <button
      type="button"
      aria-label="Zoom out"
      disabled={zoomLevel <= 0}
      onclick={() => zoomLevel--}
      class="pixel-box flex h-8 w-8 items-center justify-center bg-paper active:translate-y-[2px] active:pixel-box-pressed disabled:opacity-40"
    >
      <svg viewBox="0 0 8 8" shape-rendering="crispEdges" aria-hidden="true" class="h-4 w-4">
        <path d="M1 3h6v2h-6z" fill="var(--color-ink)" />
      </svg>
    </button>
    <!-- While a spell is selected, the button shows its shape on cyan
         instead of the book. An old message would
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
      {#if selectedShape}
        <span class="pixel-box flex h-full w-full bg-cyan p-1">
          <SpellFigure
            vertices={spellFigure(selectedShape.id, selectedShape.points)}
            label="{selectedShape.name} selected"
            class="h-full w-full"
          />
        </span>
      {:else}
        <img
          src={bookIcon}
          alt=""
          width="64"
          height="64"
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

<!-- A scan's result over the scanner. Green for a scan that counted, until
     it's tapped: its text dismisses it (to scan on), its button closes the
     scanner too. Pink for a refused or wrong one, for 2 s, untouchable so
     the scanner's own Close stays reachable. -->
{#if scanPopup}
  <div
    role="status"
    out:fade={{ duration: 200 }}
    class:pointer-events-none={!scanPopup.ok}
    class="pixel-box fixed top-1/2 left-1/2 z-[35] flex w-56 -translate-x-1/2 -translate-y-1/2 flex-col items-center gap-2 bg-white px-3 py-4 text-center font-pixel text-[8px] leading-[16px] text-ink uppercase"
  >
    {#snippet result()}
      <p class="text-[16px] leading-[24px] text-outline {scanPopup?.ok ? 'text-go' : 'text-pink'}">
        {scanPopup?.title}
      </p>
      <p>{scanPopup?.text}</p>
    {/snippet}
    {#if scanPopup.ok}
      <button
        class="flex flex-col items-center gap-2 uppercase"
        onclick={() => (scanPopup = null)}
      >
        {@render result()}
      </button>
      <button
        class="pixel-box mt-1 w-full bg-gold px-3 py-2 text-white text-outline uppercase active:translate-y-[2px] active:pixel-box-pressed"
        onclick={() => {
          clearTimeout(scanPopupTimer);
          scanPopup = null;
          closeScanner();
        }}
      >
        Close the Scanner
      </button>
    {:else}
      {@render result()}
    {/if}
  </div>
{/if}

<!-- The boss's attack: its attack animation (looping, keyed on the drop
     so another restarts it), what it took and the health left, like the
     top bar's, until the player closes it. Over everything but the toast,
     the scanner included, so it's seen wherever the player is. -->
{#if attack}
  <div
    out:fade={{ duration: 200 }}
    class="pixel-box fixed top-1/2 left-1/2 z-[45] flex w-56 -translate-x-1/2 -translate-y-1/2 flex-col items-center gap-3 bg-white px-3 py-4 font-pixel text-[8px] leading-[16px] uppercase"
  >
    {#key healthDrops}
      <BossSprite
        serial={attack.boss.serial}
        fallback={attack.boss.asset}
        animation="attack"
        loop
        class="h-24 w-auto"
      />
    {/key}
    <p class="text-center text-ink">
      {attack.boss.name} dealt {attack.dealt}
      {attack.dealt === 1 ? "heart" : "hearts"}
    </p>
    <p class="flex items-center gap-2 text-white">
      <span class="text-[18px] leading-[32px] text-outline">x{attack.health}</span>
      <img
        src={scottFace}
        alt=""
        width="64"
        height="64"
        draggable="false"
        class="h-8 w-8 [image-rendering:pixelated]"
      />
    </p>
    <button
      class="pixel-box w-full bg-gold px-3 py-2 text-white text-outline uppercase active:translate-y-[2px] active:pixel-box-pressed"
      onclick={() => (attack = null)}
    >
      Okay, I will lock in now
    </button>
  </div>
{/if}

{#if toast}
  <p
    role="status"
    class="pixel-box pointer-events-none fixed inset-x-4 top-[calc(25dvh+1rem)] z-50 mx-auto max-w-sm bg-ink px-3 py-2 text-center font-pixel text-[8px] leading-[16px] uppercase {toast.ok
      ? 'text-gold-light'
      : 'text-pink'}"
    transition:fade={{ duration: 150 }}
  >
    {toast.text}
  </p>
{/if}

<!-- A powerup's card, from tapping its icon: the icon, its name and what
     it does, then Use Now, for the leader only (others are told why). A
     tap outside or Close dismisses it; it also goes if the team stops
     holding the powerup (a teammate's leader used it). -->
{#if powerupCard && ts.powerups.held.includes(powerupCard)}
  {@const kind = powerupCard}
  <div
    class="fixed inset-0 z-40 flex items-center justify-center bg-ink/70 p-4 font-pixel text-[8px] leading-[16px] uppercase"
    transition:fade={{ duration: 150 }}
  >
    <button
      type="button"
      aria-label="Close"
      tabindex="-1"
      class="absolute inset-0"
      onclick={() => (powerupCard = null)}
    ></button>
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="powerup-title"
      class="pixel-box relative flex w-full max-w-xs flex-col items-center gap-3 bg-paper p-4 text-center text-ink"
    >
      {@render powerupIcon(kind, "h-20 w-20")}
      <h2 id="powerup-title" class="text-[16px] leading-[24px]">{powerupName(kind)}</h2>
      {#if POWERUP_DESCRIPTIONS[kind]}
        <p class="normal-case">{POWERUP_DESCRIPTIONS[kind]}</p>
      {/if}
      <button
        type="button"
        disabled={!isLeader || busy || ts.knocked_out}
        onclick={useFromCard}
        class="pixel-box w-full bg-gold px-3 py-2 text-ink uppercase active:pixel-box-pressed disabled:opacity-40"
      >
        Use Now
      </button>
      {#if !isLeader}
        <p class="opacity-60">Only your team's leader can use it.</p>
      {:else if ts.knocked_out}
        <p class="opacity-60">Knocked out: an organiser must revive your team.</p>
      {/if}
      <button
        type="button"
        onclick={() => (powerupCard = null)}
        class="pixel-box w-full bg-panel px-3 py-2 text-paper uppercase active:pixel-box-pressed"
      >
        Close
      </button>
    </div>
  </div>
{/if}

<!-- The quiz: over everything while it's open, for every player; any
     member answers for the team, once. When it closes, the right answer is
     shown. Hide keeps it running behind the "Quiz!" button. -->
{#if qte?.quiz && hiddenQTE !== qte.id}
  {@const quiz = qte.quiz}
  {@const open = qte.status === "open"}
  <div
    role="dialog"
    aria-modal="true"
    aria-labelledby="qte-question"
    class="fixed inset-0 z-40 flex items-center justify-center bg-ink/70 p-4 font-pixel text-[8px] leading-[16px] uppercase"
    transition:fade={{ duration: 150 }}
  >
    <div class="pixel-box flex max-h-full w-full max-w-sm flex-col gap-3 overflow-y-auto bg-paper p-4 text-ink">
      <p class="flex justify-between gap-2">
        <span>Quiz · win: {powerupName(qte.powerup)}</span>
        <span>{open ? countdown(qte.ends_at) : "Closed"}</span>
      </p>
      <h2 id="qte-question" class="text-[16px] leading-[24px] normal-case">{quiz.question}</h2>
      <div class="flex flex-col gap-2">
        {#each quiz.options as option, i (i)}
          {@const chosen = qteAnswer?.option === i}
          {@const right = quiz.correct_option === i}
          <button
            type="button"
            disabled={!open || !!qteAnswer || busy || ts.knocked_out}
            onclick={() => answerQTE(i)}
            class="pixel-box px-3 py-2 text-left text-paper normal-case active:pixel-box-pressed {right ||
            (chosen && qteAnswer?.correct)
              ? 'bg-go'
              : chosen
                ? 'bg-danger'
                : 'bg-panel'}"
            class:opacity-50={(!open || qteAnswer) && !chosen && !right}
          >
            {option}{chosen ? " ← your team" : ""}
          </button>
        {/each}
      </div>
      <p aria-live="polite">
        {#if qteAnswer}
          {qteAnswer.correct
            ? qteAnswer.granted
              ? `Right! ${memberName(qteAnswer.answered_by)} won ${powerupName(qte.powerup)}.`
              : `Right! But your team already holds ${powerupName(qte.powerup)}.`
            : `Wrong answer from ${memberName(qteAnswer.answered_by)}.`}
        {:else if !open}
          Time's up.
        {:else if ts.knocked_out}
          Knocked out teams can't answer.
        {:else}
          Anyone on your team can answer, once.
        {/if}
      </p>
      <button
        type="button"
        onclick={() => (hiddenQTE = qte!.id)}
        class="pixel-box bg-ink px-3 py-2 text-paper active:pixel-box-pressed"
      >
        {open ? "Hide" : "Close"}
      </button>
    </div>
  </div>
{/if}

<SpellBook
  bind:open={bookOpen}
  shapes={game.shapes}
  casts={casts}
  {selected}
  onselect={(s) => selectShape(s)}
  onclear={giveUp}
  {busy}
  {isLeader}
  canSelect={!ts.knocked_out}
  locked={ts.locked_shapes}
  cooldownShapes={ts.cooldown_shapes}
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

  <h2>2. Scan waypoints</h2>
  {#if ts.poisoned}
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
      disabled={!selected || ts.poisoned || ts.knocked_out || busy}
      >Scan a QR code</button
    >
  </p>

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
