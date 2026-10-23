<script lang="ts" module>
    // The zoom steps, as multiples of the fitted size (1 = the map's height
    // is its parent's). level indexes into these.
    export const ZOOM_LEVELS = [1, 1.25, 1.5, 2, 3, 4];

    import glow from "#lib/assets/MapIcons/glow.json";
    // How long a point's burst (glow.png's animation, played once) takes:
    // 100 ms a frame, as every marker's.
    export const BURST_MS = glow.animations.glow.frameCount * 100;
</script>

<script lang="ts">
    import mapUrl from "#lib/assets/map.webp";
    import coinsUrl from "#lib/assets/MapIcons/coins.png";
    import coins from "#lib/assets/MapIcons/coins.json";
    import glowPulseUrl from "#lib/assets/MapIcons/glow_pulse.png";
    import glowPulse from "#lib/assets/MapIcons/glow_pulse.json";
    import glowUrl from "#lib/assets/MapIcons/glow.png";
    import { MAP_SIZE, type MapPoint } from "#lib/game/mapPoints.ts";
    import { tick, untrack } from "svelte";

    // markers: the points to mark, each a spinning coin centred on its spot.
    // active: waypoint IDs whose markers glow instead (the team's scanned
    // points).
    // burst: waypoint IDs that burst instead, once (a spell just cast).
    // wrong: those of them whose scan fit no pattern; the path into one is red.
    // onpick: when set, every point is a button that calls it with its
    // waypoint (picking an instant cast's pattern).
    // cooldowns: team_state.qr_cooldowns, the points the team can't scan
    // yet; each is dimmed with a countdown until its until passes, by now
    // (the page's server clock, in ms). An expired entry is ignored, as the
    // backend drops it then (qr_cooldown_ended).
    let {
        level = $bindable(0),
        markers = [],
        active = [],
        burst = [],
        wrong = [],
        onpick,
        cooldowns = [],
        now = Date.now(),
    }: {
        level?: number;
        markers?: MapPoint[];
        active?: string[];
        burst?: string[];
        wrong?: string[];
        onpick?: (waypoint: string) => void;
        cooldowns?: { waypoint_id: string; until: string }[];
        now?: number;
    } = $props();

    // Seconds until a waypoint can be scanned again, or 0 if it can now.
    const cooldownLeft = (waypoint: string) => {
        const c = cooldowns.find((c) => c.waypoint_id === waypoint);
        return c ? Math.max(0, Math.ceil((Date.parse(c.until) - now) / 1000)) : 0;
    };
    // The attempt's outline so far, in map pixels: the scanned points in
    // order of angle around their centre, so it never crosses itself (the
    // backend matches the set of points, not the scan order), closed from 3
    // points on. A segment touching a wrong scan is red.
    let segments = $derived.by(() => {
        const pts = active.flatMap((id) => {
            const m = markers.find((m) => m.waypoint === id);
            return m ? [{ ...m, wrong: wrong.includes(id) }] : [];
        });
        const cx = pts.reduce((t, p) => t + p.x, 0) / pts.length;
        const cy = pts.reduce((t, p) => t + p.y, 0) / pts.length;
        pts.sort((p, q) => Math.atan2(p.y - cy, p.x - cx) - Math.atan2(q.y - cy, q.x - cx));
        const n = pts.length < 3 ? pts.length - 1 : pts.length;
        return Array.from({ length: Math.max(0, n) }, (_, i) => {
            const a = pts[i], b = pts[(i + 1) % pts.length];
            return { a, b, wrong: a.wrong || b.wrong };
        });
    });
    // A marker's look, from a spritesheet laid out by its JSON: the
    // animation's frames sit in a row at startX + i × stride, each sprite
    // centred in its frame, so a box round every frame's sprite, as wide as
    // a frame, is centred on it. Shown px wide and stepped through the
    // frames by CSS (a background sliding a whole stride each step), one
    // 100 ms frame each; see .sprite.
    type Sheet = {
        imageWidth: number;
        imageHeight: number;
        animations: Record<string, {
            frameCount: number;
            startX: number;
            stride: number;
            frameWidth: number;
            frames: { sprite: { y: number; h: number } }[];
        }>;
    };
    const sheetStyle = (sheet: Sheet, url: string, name: string, px: number) => {
        const anim = sheet.animations[name];
        const top = Math.min(...anim.frames.map((f) => f.sprite.y));
        const bottom = Math.max(...anim.frames.map((f) => f.sprite.y + f.sprite.h));
        const k = px / anim.frameWidth;
        return [
            `--sprite-w: ${px}px`,
            `--sprite-h: ${(bottom - top) * k}px`,
            `--sprite-sheet: url(${url})`,
            `--sprite-size: ${sheet.imageWidth * k}px ${sheet.imageHeight * k}px`,
            `--sprite-x0: ${-anim.startX * k}px`,
            `--sprite-x1: ${-(anim.startX + anim.frameCount * anim.stride) * k}px`,
            `--sprite-y: ${-top * k}px`,
            `--sprite-steps: ${anim.frameCount}`,
            `--sprite-ms: ${anim.frameCount * 100}ms`,
        ].join(";");
    };
    // A point: a spinning coin, 24px across. A scanned one: a pulsing glow
    // instead, at its biggest a little wider than the coin.
    const coinStyle = sheetStyle(coins, coinsUrl, "spin", 24);
    const glowStyle = sheetStyle(glowPulse, glowPulseUrl, "pulse", 36);
    // A cast's burst, at its biggest about three coins across.
    const burstStyle = sheetStyle(glow, glowUrl, "glow", 72);

    const mmss = (s: number) => `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
    let zoom = $derived(ZOOM_LEVELS[Math.max(0, Math.min(level, ZOOM_LEVELS.length - 1))]);

    // The map fills its parent, as tall as it (fit to height) times zoom, and
    // pans on both axes, stopping at its edges. Touch pans with the browser's
    // own scrolling (momentum included); a mouse drags it with pointer
    // events. Either way it moves scrollLeft/scrollTop, which the browser
    // clamps to the map's borders.
    let viewport = $state<HTMLDivElement>();
    // The map and its markers, sized together, so the markers move with it.
    let map = $state<HTMLDivElement>();
    let drag: { x: number; y: number; left: number; top: number } | null = null;

    const down = (e: PointerEvent) => {
        if (e.pointerType !== "mouse" || e.button !== 0 || !viewport) return;
        // A press on a point's button (picking) is a click, not a drag:
        // capturing it would send the click to the map instead.
        if ((e.target as Element).closest("button")) return;
        drag = { x: e.clientX, y: e.clientY, left: viewport.scrollLeft, top: viewport.scrollTop };
        viewport.setPointerCapture(e.pointerId);
    };
    const move = (e: PointerEvent) => {
        if (!drag || !viewport) return;
        viewport.scrollLeft = drag.left - (e.clientX - drag.x);
        viewport.scrollTop = drag.top - (e.clientY - drag.y);
    };
    const up = () => (drag = null);

    // Start centred, like the map was before it moved.
    const centre = () => {
        if (!viewport) return;
        viewport.scrollLeft = (viewport.scrollWidth - viewport.clientWidth) / 2;
        viewport.scrollTop = (viewport.scrollHeight - viewport.clientHeight) / 2;
    };

    // Zooming keeps the point at the middle of the view where it is: before
    // the map resizes, note that point as a fraction of the map; after,
    // scroll it back to the middle (clamped at the edges, as ever).
    $effect.pre(() => {
        zoom;
        untrack(() => {
            if (!viewport || !map) return;
            const v = viewport, m = map;
            const fx = (v.scrollLeft + v.clientWidth / 2 - m.offsetLeft) / m.offsetWidth;
            const fy = (v.scrollTop + v.clientHeight / 2 - m.offsetTop) / m.offsetHeight;
            tick().then(() => {
                v.scrollLeft = m.offsetLeft + fx * m.offsetWidth - v.clientWidth / 2;
                v.scrollTop = m.offsetTop + fy * m.offsetHeight - v.clientHeight / 2;
            });
        });
    });
</script>

<div
    bind:this={viewport}
    role="region"
    aria-label="Map"
    class="h-full w-full cursor-grab touch-pan-x touch-pan-y overflow-auto overscroll-none select-none [scrollbar-width:none] active:cursor-grabbing [&::-webkit-scrollbar]:hidden"
    onpointerdown={down}
    onpointermove={move}
    onpointerup={up}
    onpointercancel={up}
>
    <!-- The map's box: as tall as the viewport × zoom, as wide as the
         image's shape makes that. Markers are placed in it by percentage,
         so zooming and panning carry them along. -->
    <div
        bind:this={map}
        class="relative mx-auto max-w-none"
        style:height="{zoom * 100}%"
        style:aspect-ratio="{MAP_SIZE.width} / {MAP_SIZE.height}"
    >
        <img
            src={mapUrl}
            alt="Map of the labyrinth"
            width={MAP_SIZE.width}
            height={MAP_SIZE.height}
            draggable="false"
            onload={centre}
            class="block h-full w-full [image-rendering:pixelated]"
        />
        <!-- The path through the points scanned so far, under the markers.
             In map pixels, stretched with the map; the strokes stay the
             same width on screen at every zoom. No outline: the glow
             pulse's white core, haloed by two wider, fainter strokes of
             its pale yellow rim (red for a wrong scan's path). -->
        <svg
            viewBox="0 0 {MAP_SIZE.width} {MAP_SIZE.height}"
            preserveAspectRatio="none"
            aria-hidden="true"
            class="pointer-events-none absolute inset-0 h-full w-full"
        >
            {#each segments as { a, b, wrong } (`${a.waypoint}-${b.waypoint}`)}
                {@const glow = wrong ? "var(--color-danger)" : "#f9f3a3"}
                <line x1={a.x} y1={a.y} x2={b.x} y2={b.y} stroke={glow} stroke-opacity="0.15" stroke-width="14" stroke-linecap="round" vector-effect="non-scaling-stroke" />
                <line x1={a.x} y1={a.y} x2={b.x} y2={b.y} stroke={glow} stroke-opacity="0.35" stroke-width="8" stroke-linecap="round" vector-effect="non-scaling-stroke" />
                <line x1={a.x} y1={a.y} x2={b.x} y2={b.y} stroke={wrong ? "var(--color-danger)" : "#f7f8f8"} stroke-width="4" stroke-linecap="round" vector-effect="non-scaling-stroke" />
            {/each}
        </svg>
        <!-- Each marker a coin, a scanned point's glow or a cast's burst,
             its centre on its point. The same size at every zoom; taps pass through to the
             map. A point on QR cooldown is dimmed, its countdown above it. -->
        {#each markers as m (m.name)}
            {@const left = cooldownLeft(m.waypoint)}
            <span
                role="img"
                aria-label={left > 0 ? `${m.name}, on cooldown` : m.name}
                style:left="{(m.x / MAP_SIZE.width) * 100}%"
                style:top="{(m.y / MAP_SIZE.height) * 100}%"
                class:opacity-50={left > 0}
                class:grayscale={left > 0}
                class="pointer-events-none absolute -translate-x-1/2 -translate-y-1/2"
            >
                {#if burst.includes(m.waypoint)}
                    <span class="sprite once block" style={burstStyle}></span>
                {:else}
                    <span
                        class="sprite block"
                        style={active.includes(m.waypoint) ? glowStyle : coinStyle}
                    ></span>
                {/if}
            </span>
            {#if onpick}
                <!-- A 44px target centred on the marker. -->
                <button
                    type="button"
                    aria-label="Pick {m.name}"
                    aria-pressed={active.includes(m.waypoint)}
                    onclick={() => onpick(m.waypoint)}
                    style:left="{(m.x / MAP_SIZE.width) * 100}%"
                    style:top="{(m.y / MAP_SIZE.height) * 100}%"
                    class="absolute h-11 w-11 -translate-x-1/2 -translate-y-1/2 rounded-full border-2 border-dashed border-gold-light"
                ></button>
            {/if}
            {#if left > 0}
                <span
                    aria-hidden="true"
                    style:left="{(m.x / MAP_SIZE.width) * 100}%"
                    style:top="{(m.y / MAP_SIZE.height) * 100}%"
                    class="pointer-events-none absolute -translate-x-1/2 translate-y-[calc(-100%-14px)] bg-ink px-1 font-pixel text-[8px] leading-[12px] whitespace-nowrap text-paper"
                >
                    {mmss(left)}
                </span>
            {/if}
        {/each}
    </div>
</div>

<style>
    /* A marker, looping through its sheet's frames (sheetStyle); still on
       its first with reduced motion. */
    .sprite {
        width: var(--sprite-w);
        height: var(--sprite-h);
        background: var(--sprite-sheet) no-repeat var(--sprite-x0) var(--sprite-y) / var(--sprite-size);
        animation: sprite-loop var(--sprite-ms) steps(var(--sprite-steps)) infinite;
    }
    /* Played once, then gone: it ends a stride past the last frame. */
    .sprite.once {
        animation-iteration-count: 1;
        animation-fill-mode: forwards;
    }
    @keyframes sprite-loop {
        to {
            background-position-x: var(--sprite-x1);
        }
    }
    @media (prefers-reduced-motion: reduce) {
        .sprite {
            animation: none;
        }
    }
</style>
