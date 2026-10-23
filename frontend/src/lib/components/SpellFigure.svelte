<script lang="ts">
    // A spell's shape as pixel art: glowing yellow vertices, each bobbing a
    // pixel on its own beat, joined in order (and back to the first, for 3+)
    // by stepped yellow lines. Drawn on a 32×32 grid that scales with the
    // box; the lines are rasterised from the vertices' current positions,
    // so they follow the bob without gaps. Still with reduced motion.
    import { onMount } from "svelte";

    let {
        vertices,
        label,
        class: className = "",
    }: {
        // Grid coordinates, 0–31, kept 3 pixels in from the edge so a
        // vertex's glow and bob fit.
        vertices: [number, number][];
        label: string;
        class?: string;
    } = $props();

    const STEP_MS = 160;
    let tick = $state(0);

    onMount(() => {
        if (matchMedia("(prefers-reduced-motion: reduce)").matches) return;
        const id = setInterval(() => tick++, STEP_MS);
        return () => clearInterval(id);
    });

    // Each vertex's offset is -1, 0 or 1 pixels, out of step with the rest.
    let placed = $derived(
        vertices.map(([x, y], i): [number, number] => [x, y + Math.round(Math.sin(tick * 0.5 + i * 2.1))]),
    );

    // Bresenham's line: every pixel from a to b, as "x,y" keys.
    const rasterise = ([x0, y0]: [number, number], [x1, y1]: [number, number], into: Set<string>) => {
        const dx = Math.abs(x1 - x0);
        const dy = -Math.abs(y1 - y0);
        const sx = x0 < x1 ? 1 : -1;
        const sy = y0 < y1 ? 1 : -1;
        let err = dx + dy;
        for (;;) {
            into.add(`${x0},${y0}`);
            if (x0 === x1 && y0 === y1) return;
            const e2 = 2 * err;
            if (e2 >= dy) {
                err += dy;
                x0 += sx;
            }
            if (e2 <= dx) {
                err += dx;
                y0 += sy;
            }
        }
    };

    // Every edge's pixels as one path of 1×1 squares.
    let lines = $derived.by(() => {
        const pixels = new Set<string>();
        const edges = placed.length > 2 ? placed.length : placed.length - 1;
        for (let i = 0; i < edges; i++) rasterise(placed[i], placed[(i + 1) % placed.length], pixels);
        return [...pixels].map((p) => `M${p.replace(",", " ")}h1v1h-1z`).join("");
    });
</script>

<svg viewBox="0 0 32 32" shape-rendering="crispEdges" role="img" aria-label={label} class="spell-figure {className}">
    <path d={lines} fill="var(--color-gold-light)" />
    {#each placed as [x, y], i (i)}
        <!-- A vertex: a pulsing two-step halo round a pale core. -->
        <g class="vertex" style:animation-delay="{i * -0.37}s">
            <rect x={x - 3} y={y - 3} width="7" height="7" fill="var(--color-gold-light)" opacity="0.15" />
            <rect x={x - 2} y={y - 2} width="5" height="5" fill="var(--color-gold-light)" opacity="0.35" />
        </g>
        <rect x={x - 1} y={y - 1} width="3" height="3" fill="#fff6c8" />
    {/each}
</svg>

<style>
    /* The whole figure glows; the halos swell and fade on their own. */
    .spell-figure {
        filter: drop-shadow(0 0 4px var(--color-gold-light));
    }
    .vertex {
        animation: pulse 1.2s ease-in-out infinite;
    }
    @keyframes pulse {
        0%,
        100% {
            opacity: 0.5;
        }
        50% {
            opacity: 1;
        }
    }
    @media (prefers-reduced-motion: reduce) {
        .vertex {
            animation: none;
        }
    }
</style>
