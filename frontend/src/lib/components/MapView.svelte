<script lang="ts" module>
    // The zoom steps, as multiples of the fitted size (1 = the map's height
    // is its parent's). level indexes into these.
    export const ZOOM_LEVELS = [1, 1.5, 2, 3, 4];
</script>

<script lang="ts">
    import mapUrl from "#lib/assets/map.webp";
    import { tick, untrack } from "svelte";

    let { level = $bindable(0) }: { level?: number } = $props();
    let zoom = $derived(ZOOM_LEVELS[Math.max(0, Math.min(level, ZOOM_LEVELS.length - 1))]);

    // The map fills its parent, as tall as it (fit to height) times zoom, and
    // pans on both axes, stopping at its edges. Touch pans with the browser's
    // own scrolling (momentum included); a mouse drags it with pointer
    // events. Either way it moves scrollLeft/scrollTop, which the browser
    // clamps to the map's borders.
    let viewport = $state<HTMLDivElement>();
    let img = $state<HTMLImageElement>();
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
            if (!viewport || !img) return;
            const v = viewport, m = img;
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
    <img
        bind:this={img}
        src={mapUrl}
        alt="Map of the labyrinth"
        width="1168"
        height="1346"
        draggable="false"
        onload={centre}
        style:height="{zoom * 100}%"
        class="mx-auto block w-auto max-w-none [image-rendering:pixelated]"
    />
</div>
