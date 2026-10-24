<script lang="ts">
    // The Scott Pilgrim menu's backdrop: a checkerboard of two blues turned
    // 45° into diamonds, lit from the top right and shaded towards the bottom
    // left. It fills its nearest positioned ancestor; class places it (e.g.
    // "fixed inset-0 -z-10" behind a page, "absolute inset-0" in a bar).
    // colors: the two tiles' colours, [dark, light], instead of the blues.
    let {
        colors = ["var(--color-tile-dark)", "var(--color-tile-light)"],
        class: className = "",
    }: { colors?: [string, string]; class?: string } = $props();
</script>

<div
    aria-hidden="true"
    style:--tile-dark={colors[0]}
    style:--tile-light={colors[1]}
    class="pointer-events-none overflow-hidden bg-(--tile-dark) {className}"
>
    <div
        class="absolute top-1/2 left-1/2 h-[200vmax] w-[200vmax] -translate-x-1/2 -translate-y-1/2 rotate-45 bg-[repeating-conic-gradient(var(--tile-dark)_0_25%,var(--tile-light)_0_50%)] bg-[length:64px_64px]"
    ></div>
    <div
        class="absolute inset-0 bg-[radial-gradient(ellipse_at_85%_5%,rgb(255_255_255/0.3),transparent_55%),linear-gradient(to_top_right,rgb(0_0_40/0.5),transparent_65%)]"
    ></div>
</div>
