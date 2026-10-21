<script lang="ts" module>
    // The zoom steps, as multiples of the fitted size (1 = the map's height
    // is its parent's). level indexes into these.
    export const ZOOM_LEVELS = [1, 1.5, 2, 3, 4];
</script>

<script lang="ts">
    import mapUrl from "#lib/assets/map.webp";
    import markerUrl from "#lib/assets/MapIcons/down-arrow.png";
    import { MAP_SIZE, type MapPoint } from "#lib/game/mapPoints.ts";
    import { tick, untrack } from "svelte";

    // markers: the points to mark, each drawn with its tip on its spot.
    // active: waypoint IDs whose markers wiggle (the team's scanned points).
    // cooldowns: team_state.qr_cooldowns, the points the team can't scan
    // yet; each is dimmed with a countdown until its until passes, by now
    // (the page's server clock, in ms). An expired entry is ignored, as the
    // backend drops it then (qr_cooldown_ended).
    let {
        level = $bindable(0),
        markers = [],
        active = [],
        cooldowns = [],
        now = Date.now(),
    }: {
        level?: number;
        markers?: MapPoint[];
        active?: string[];
        cooldowns?: { waypoint_id: string; until: string }[];
        now?: number;
    } = $props();

    // Seconds until a waypoint can be scanned again, or 0 if it can now.
    const cooldownLeft = (waypoint: string) => {
        const c = cooldowns.find((c) => c.waypoint_id === waypoint);
        return c ? Math.max(0, Math.ceil((Date.parse(c.until) - now) / 1000)) : 0;
    };
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
        <!-- Each marker's bottom-centre (the arrow's tip) on its point. The
             same size at every zoom; taps pass through to the map. A point
             on QR cooldown is dimmed, its countdown above the arrow. -->
        {#each markers as m (m.name)}
            {@const left = cooldownLeft(m.waypoint)}
            <img
                src={markerUrl}
                alt={left > 0 ? `${m.name}, on cooldown` : m.name}
                width="16"
                height="16"
                draggable="false"
                style:left="{(m.x / MAP_SIZE.width) * 100}%"
                style:top="{(m.y / MAP_SIZE.height) * 100}%"
                class:animate-wiggle={active.includes(m.waypoint)}
                class:opacity-50={left > 0}
                class:grayscale={left > 0}
                class="pointer-events-none absolute h-4 w-4 max-w-none -translate-x-1/2 -translate-y-full [image-rendering:pixelated]"
            />
            {#if left > 0}
                <span
                    aria-hidden="true"
                    style:left="{(m.x / MAP_SIZE.width) * 100}%"
                    style:top="{(m.y / MAP_SIZE.height) * 100}%"
                    class="pointer-events-none absolute -translate-x-1/2 translate-y-[calc(-100%-18px)] bg-ink px-1 font-pixel text-[8px] leading-[12px] whitespace-nowrap text-paper"
                >
                    {mmss(left)}
                </span>
            {/if}
        {/each}
    </div>
</div>
