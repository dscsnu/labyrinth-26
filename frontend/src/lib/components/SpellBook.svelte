<script lang="ts">
    import SpellFigure from "#lib/components/SpellFigure.svelte";
    import { spellFigure } from "#lib/game/spellFigure.ts";
    import type { Cast, GameShape } from "#lib/types/Game.type.ts";
    import { untrack } from "svelte";
    import { fade, fly } from "svelte/transition";

    // The spell book: a sheet of paper that slides up over 90% of the screen.
    // Each shape is a page, fewest points first: its shape and rules (points,
    // damage range, and how many of its patterns the team has found, from
    // the cast log, so it counts up live as anyone on the team casts). A
    // shape split into damage bands (VARIANTS) gets a tab per band on its
    // page, each with its own rules and count. The arrows at the
    // bottom turn the pages; close is between them. The leader also gets a
    // button on each page to select its spell, or clear the selection on the
    // selected one's page; the book only raises onselect/onclear, and the
    // page makes the request and passes back busy and its message.
    let {
        open = $bindable(false),
        shapes,
        casts,
        selected = null,
        onselect,
        onclear,
        busy = false,
        isLeader = false,
        canSelect = true,
        locked = [],
        cooldownShapes = [],
        message = null,
    }: {
        open?: boolean;
        shapes: GameShape[];
        casts: Cast[];
        selected?: string | null;
        onselect: (shape: string) => void;
        onclear: () => void;
        // A selection request is in flight.
        busy?: boolean;
        // Only the leader sees the buttons; the backend refuses anyone else.
        isLeader?: boolean;
        // False while the team is knocked out: it can't select.
        canSelect?: boolean;
        // ts.locked_shapes: shapes whose patterns are all still locked.
        locked?: string[];
        // ts.cooldown_shapes: shapes blocked because every pattern of them
        // is of the last cast's class (no same class twice in a row).
        cooldownShapes?: string[];
        // The last request's result, shown under the button.
        message?: { ok: boolean; text: string } | null;
    } = $props();

    // Each shape's discovered patterns, once each: the spell's damage, which
    // says its band.
    let discovered = $derived.by(() => {
        const byShape = new Map<string, Map<string, { damage: number }>>();
        for (const c of casts) {
            const patterns = byShape.get(c.shape) ?? new Map();
            byShape.set(c.shape, patterns);
            patterns.set(c.pattern.join("-"), { damage: c.damage });
        }
        return new Map([...byShape].map(([shape, patterns]) => [shape, [...patterns.values()]]));
    });

    // Shapes whose patterns fall into separate damage bands, each its own
    // tab. The backend sends one range per shape, so these mirror
    // backend/internal/spells/README.md §5: concave quadrilaterals get
    // spell.bonus.concave_quad on top. Update them if [spell] in
    // config.toml changes. The bands don't overlap, so a cast's damage
    // says which one it's in.
    const VARIANTS: Record<string, { name: string; damage: { min: number; max: number }; patterns: number }[]> = {
        quadrilateral: [
            { name: "Convex", damage: { min: 195, max: 205 }, patterns: 156 },
            { name: "Concave", damage: { min: 270, max: 278 }, patterns: 54 },
        ],
    };

    type Tab = {
        name: string;
        damage: { min: number; max: number };
        patterns: number;
        matches: (damage: number) => boolean;
    };
    type Page = { shapeId: string; name: string; points: number[]; tabs: Tab[] };

    // One page per shape, by its fewest points (the backend sends them by
    // id), with a tab per band of a shape in VARIANTS, else one holding the
    // whole shape. A band holds the casts whose damage reaches its min and
    // not the next band's.
    let pages = $derived(
        [...shapes]
            .sort((a, b) => Math.min(...a.points) - Math.min(...b.points) || a.name.localeCompare(b.name))
            .map((s): Page => {
                const variants = VARIANTS[s.id];
                const tabs: Tab[] = variants
                    ? variants.map((v, i) => ({
                          name: v.name,
                          damage: v.damage,
                          patterns: v.patterns,
                          matches: (d) =>
                              (i === 0 || d >= v.damage.min) && (i === variants.length - 1 || d < variants[i + 1].damage.min),
                      }))
                    : [{ name: s.name, damage: s.damage, patterns: s.patterns, matches: () => true }];
                return { shapeId: s.id, name: s.name, points: s.points, tabs };
            }),
    );

    // The page shown (an index into pages), and which way the last turn
    // went, so the new page slides in from that side; and its tab, the
    // first again on every turn.
    let page = $state(0);
    let dir = $state(1);
    let tab = $state(0);
    let shape = $derived(pages[Math.min(page, pages.length - 1)]);
    let current = $derived(shape?.tabs[Math.min(tab, shape.tabs.length - 1)]);
    let known = $derived(
        shape && current ? (discovered.get(shape.shapeId) ?? []).filter((p) => current.matches(p.damage)) : [],
    );

    // Opening the book turns to the team's selected shape, if it has one;
    // otherwise to the page it was left on. Only on opening: a teammate
    // selecting while it's open doesn't turn the reader's page.
    $effect(() => {
        if (!open) return;
        const i = untrack(() => pages.findIndex((p) => p.shapeId === selected));
        if (i >= 0 && i !== untrack(() => page)) {
            page = i;
            tab = 0;
        }
    });

    const turn = (by: number) => {
        const next = page + by;
        if (next < 0 || next >= pages.length) return;
        dir = by;
        page = next;
        tab = 0;
    };
    const close = () => (open = false);

    // One-finger swipes on the book: left turns to the next page, right to
    // the previous, down closes it, if the page was already scrolled to its
    // top (or the swipe began outside it, on the header or buttons), so
    // scrolling back up never closes it by accident. A swipe must travel
    // at least SWIPE_PX, mostly in its direction. Touch events, not pointer
    // ones: those are cancelled once the browser starts scrolling.
    const SWIPE_PX = 48;
    let scroller = $state<HTMLElement | null>(null);
    let swipeStart: { x: number; y: number; atTop: boolean } | null = null;
    const touchStart = (e: TouchEvent) => {
        if (e.touches.length !== 1) {
            swipeStart = null;
            return;
        }
        const t = e.touches[0];
        const onPage = scroller?.contains(e.target as Node) ?? false;
        swipeStart = { x: t.clientX, y: t.clientY, atTop: !onPage || (scroller?.scrollTop ?? 0) <= 0 };
    };
    const touchEnd = (e: TouchEvent) => {
        if (!swipeStart) return;
        const t = e.changedTouches[0];
        const dx = t.clientX - swipeStart.x;
        const dy = t.clientY - swipeStart.y;
        const { atTop } = swipeStart;
        swipeStart = null;
        if (Math.abs(dx) >= SWIPE_PX && Math.abs(dx) > 1.5 * Math.abs(dy)) turn(dx < 0 ? 1 : -1);
        else if (atTop && dy >= SWIPE_PX && dy > 1.5 * Math.abs(dx)) close();
    };

    const keydown = (e: KeyboardEvent) => {
        if (!open) return;
        if (e.key === "Escape") close();
        else if (e.key === "ArrowLeft") turn(-1);
        else if (e.key === "ArrowRight") turn(1);
    };

    const navButton =
        "pixel-box bg-ink px-3 py-2 text-paper uppercase active:pixel-box-pressed disabled:pointer-events-none disabled:opacity-30";
</script>

<svelte:window onkeydown={keydown} />

{#if open}
    <!-- Dims the map; a tap outside the page closes the book. -->
    <button
        type="button"
        aria-label="Close spell book"
        tabindex="-1"
        class="fixed inset-0 z-20 bg-ink/60"
        onclick={close}
        transition:fade={{ duration: 150 }}
    ></button>

    <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="spellbook-title"
        tabindex="-1"
        ontouchstart={touchStart}
        ontouchend={touchEnd}
        ontouchcancel={() => (swipeStart = null)}
        class="fixed inset-x-0 bottom-0 z-30 flex h-[90dvh] flex-col overflow-hidden bg-paper font-pixel text-[16px] leading-[24px] text-ink shadow-[0_-4px_0_0_var(--color-ink)]"
        transition:fly={{ y: "100%", duration: 250, opacity: 1 }}
    >
        <header class="flex items-center justify-between border-b-4 border-ink px-4 py-3">
            <h2 id="spellbook-title" class="uppercase">Spell book</h2>
            {#if pages.length > 0}
                <span class="text-[8px] leading-[16px]">{page + 1} / {pages.length}</span>
            {/if}
        </header>

        <!-- The page, ruled like notebook paper, one rule per 24px line.
             Keyed by page so each turn slides the new one in. -->
        <div
            bind:this={scroller}
            class="relative flex-1 overflow-y-auto overscroll-contain bg-[repeating-linear-gradient(transparent_0_23px,var(--color-paper-line)_23px_24px)] px-4 pt-4 pb-8"
        >
            {#if shape}
                {#key page}
                    <section class="mx-auto max-w-md" in:fly={{ x: dir * 48, duration: 200 }}>
                        <!-- The title, centred across the page. -->
                        <h3 class="flex flex-wrap items-center justify-center gap-2 text-center text-[24px] leading-[32px] uppercase">
                            {shape.name}
                            {#if shape.shapeId === selected}
                                <span class="bg-gold px-2 text-[8px] leading-[16px]">Selected</span>
                            {/if}
                            {#if cooldownShapes.includes(shape.shapeId)}
                                <span class="bg-ink px-2 text-[8px] leading-[16px] text-paper">Cooldown</span>
                            {/if}
                        </h3>
                        <!-- A shape's bands as tabs: same spell, same select
                             button, each its own damage and found count. -->
                        {#if shape.tabs.length > 1}
                            <div role="tablist" aria-label="{shape.name} types" class="mt-4 flex justify-center gap-2">
                                {#each shape.tabs as t, i (t.name)}
                                    <button
                                        type="button"
                                        role="tab"
                                        aria-selected={t === current}
                                        onclick={() => (tab = i)}
                                        class="pixel-box px-3 py-1 text-[8px] leading-[16px] uppercase active:pixel-box-pressed {t === current
                                            ? 'bg-ink text-paper'
                                            : 'bg-paper text-ink'}"
                                    >
                                        {t.name}
                                    </button>
                                {/each}
                            </div>
                        {/if}
                        <!-- The spell's shape (the open tab's): a centred square,
                             half the page's width but at most a quarter of the
                             screen's height, so the details below stay in view on a
                             wide or short screen. Dark, as yellow barely shows on
                             the paper. -->
                        <div class="pixel-box mx-auto mt-4 aspect-square w-[min(50%,25dvh)] bg-night p-1">
                            <SpellFigure
                                vertices={spellFigure(shape.shapeId, shape.points, current?.name)}
                                label="{shape.tabs.length > 1 && current ? `${current.name} ` : ''}{shape.name} shape"
                                class="block h-full w-full"
                            />
                        </div>
                        <dl class="mt-4 grid grid-cols-[auto_1fr] gap-x-4 text-[8px] leading-[24px] uppercase">
                            <dt class="opacity-60">Points</dt>
                            <dd>{shape.points.join(" or ")}</dd>
                            {#if current}
                                <dt class="opacity-60">Damage range</dt>
                                <dd>{current.damage.min === current.damage.max ? current.damage.min : `${current.damage.min} to ${current.damage.max}`}</dd>
                                <dt class="opacity-60">Found</dt>
                                <dd>{known.length} / {current.patterns} patterns</dd>
                            {/if}
                        </dl>
                    </section>
                {/key}
            {:else}
                <p class="text-[8px] leading-[16px] uppercase">No spells loaded.</p>
            {/if}
        </div>

        <!-- The leader's button, pinned under the page so it's never
             scrolled out of view: clear on the selected spell's page (on
             every tab: the backend selects the shape, not the band),
             select on the others, disabled while another spell is
             selected. -->
        {#if isLeader && shape}
            {@const isSelected = shape.shapeId === selected}
            {@const isLocked = locked.includes(shape.shapeId)}
            {@const isCooling = cooldownShapes.includes(shape.shapeId)}
            <div class="border-t-4 border-ink px-4 py-3">
                <div class="mx-auto flex max-w-md flex-col gap-2">
                    {#if isSelected}
                        <button type="button" disabled={busy || !canSelect} onclick={onclear} class="{navButton} w-full text-[8px] leading-[16px]">
                            Clear selection
                        </button>
                    {:else}
                        <button
                            type="button"
                            disabled={busy || !canSelect || selected !== null || isLocked || isCooling}
                            onclick={() => onselect(shape.shapeId)}
                            class="{navButton} w-full text-[8px] leading-[16px]"
                        >
                            Select Spell
                        </button>
                    {/if}
                    {#if !canSelect}
                        <p class="text-[8px] leading-[16px] uppercase opacity-60">Knocked out: an organiser must revive your team.</p>
                    {:else if isLocked && !isSelected}
                        <p class="text-[8px] leading-[16px] uppercase opacity-60">Locked until your team defeats more bosses.</p>
                    {:else if isCooling && !isSelected}
                        <p class="text-[8px] leading-[16px] uppercase opacity-60">Cooldown: cast a spell of another class first.</p>
                    {/if}
                    {#if message}
                        <p aria-live="polite" class="text-[8px] leading-[16px] uppercase" class:text-pink={!message.ok} class:text-cyan={message.ok}>
                            {message.text}
                        </p>
                    {/if}
                </div>
            </div>
        {/if}

        <!-- Previous page (left), close (middle), next page (right). -->
        <footer class="grid grid-cols-3 items-center border-t-4 border-ink px-4 py-4 pb-[max(1rem,env(safe-area-inset-bottom))]">
            <button type="button" aria-label="Previous page" disabled={page <= 0} onclick={() => turn(-1)} class="{navButton} justify-self-start">
                &lt;
            </button>
            <button type="button" onclick={close} class="{navButton} justify-self-center text-[8px] leading-[16px]">Close</button>
            <button
                type="button"
                aria-label="Next page"
                disabled={page >= pages.length - 1}
                onclick={() => turn(1)}
                class="{navButton} justify-self-end"
            >
                &gt;
            </button>
        </footer>
    </div>
{/if}
