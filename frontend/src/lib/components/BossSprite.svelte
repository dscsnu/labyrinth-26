<script lang="ts" module>
    import { loadImage } from "#lib/game/preload.ts";
    import { z } from "zod";

    // Only the fields used here.
    const BoxSchema = z.object({ x: z.number(), y: z.number(), w: z.number(), h: z.number() });
    const FrameSchema = BoxSchema.extend({ sprite: BoxSchema });
    const SheetSchema = z.object({
        imageWidth: z.number(),
        imageHeight: z.number(),
        animations: z
            .record(z.string(), z.object({ loop: z.boolean(), frames: z.array(FrameSchema).min(1) }))
            .refine((a) => "idle" in a, "no idle animation"),
    });

    type Sheet = {
        width: number;
        height: number;
        animations: Record<string, { loop: boolean; frames: z.infer<typeof FrameSchema>[] }>;
        // One box, relative to a frame's top-left corner, round every idle frame's
        // sprite: frames are mostly empty above the boss, and sharing a box
        // keeps its baseline still. Every animation is drawn at its scale;
        // bigger ones (death's beam) spill out of it, clipped to their frame.
        crop: { x: number; y: number; w: number; h: number };
    };

    export const sheetUrl = (serial: number) => `/bosses_animation/${serial}.png`;
    export const layoutUrl = (serial: number) => `/bosses_animation/${serial}.json`;

    // One load per boss: its JSON and its image fetched side by side,
    // resolving once both are in and the image decoded, so the sprite never
    // shows before its pixels. Loaded sheets are kept in ready, for a sprite
    // mounted later (the next boss, the attack) to show on its first frame;
    // a failed load is forgotten, so the next call tries again.
    const loads = new Map<number, Promise<Sheet>>();
    const ready = new Map<number, Sheet>();

    export const preloadBoss = (serial: number): Promise<Sheet> => {
        let p = loads.get(serial);
        if (p) return p;
        const layout = fetch(layoutUrl(serial))
            .then((res) => (res.ok ? res.json() : Promise.reject(res.status)))
            .then((json) => SheetSchema.parse(json));
        p = Promise.all([layout, loadImage(sheetUrl(serial))]).then(
            ([{ imageWidth, imageHeight, animations }]) => {
                const idle = animations.idle.frames;
                const left = Math.min(...idle.map((f) => f.sprite.x - f.x));
                const top = Math.min(...idle.map((f) => f.sprite.y - f.y));
                const right = Math.max(...idle.map((f) => f.sprite.x - f.x + f.sprite.w));
                const bottom = Math.max(...idle.map((f) => f.sprite.y - f.y + f.sprite.h));
                const sheet = {
                    width: imageWidth,
                    height: imageHeight,
                    animations,
                    crop: { x: left, y: top, w: right - left, h: bottom - top },
                };
                ready.set(serial, sheet);
                return sheet;
            },
        );
        p.catch(() => loads.delete(serial));
        loads.set(serial, p);
        return p;
    };
</script>

<script lang="ts">
    // A boss, animated from static/bosses_animation/<serial>.png, the
    // spritesheet its <serial>.json lays out: animation loops if the sheet
    // says so (or loop does), else plays once, holds its last frame and calls onend; a
    // change to replay plays it again from the start.
    // Until the sheet is loaded it shows the boss's still (bossStills.ts),
    // the same size as its first frame, so the swap doesn't move anything.
    // Without a sheet (missing or malformed JSON) it falls back to the still
    // static/<fallback>, hidden if that's missing too; then, as without the
    // animation or with reduced motion, onend is called at once.
    import { BOSS_FALLBACKS, BOSS_STILLS, bossStillUrl } from "#lib/game/bossStills.ts";
    import { untrack } from "svelte";

    let {
        serial,
        fallback,
        animation = "idle",
        replay = 0,
        loop,
        onend,
        class: className = "",
    }: {
        serial: number;
        fallback: string;
        animation?: string;
        replay?: number;
        // Overrides the sheet's loop for this animation.
        loop?: boolean;
        onend?: () => void;
        class?: string;
    } = $props();

    // The JSON has no timing.
    const FRAME_MS = 125;

    const clipId = $props.id();
    // Raw: a sheet is never changed, only replaced, and a proxy of it would
    // differ from the cached one on every assignment.
    let sheet = $state.raw<Sheet | null>(untrack(() => ready.get(serial) ?? null));
    let failed = $state(false);
    let frame = $state(0);
    let hidden = $state(false);
    let stillHidden = $state(false);
    let still = $derived(BOSS_STILLS[serial]);

    $effect(() => {
        const s = serial;
        const cached = ready.get(s) ?? null;
        sheet = cached;
        failed = false;
        if (cached) return;
        let cancelled = false;
        preloadBoss(s).then(
            (loaded) => {
                if (!cancelled) sheet = loaded;
            },
            () => {
                if (!cancelled) failed = true;
            },
        );
        return () => {
            cancelled = true;
        };
    });

    let current = $derived(sheet?.animations[animation] ?? null);

    // Plays the animation from its first frame whenever it (or the sheet, or
    // replay) changes. Nothing to play ends it at once, so a caller waiting
    // on onend is never stuck.
    $effect(() => {
        const anim = current;
        replay;
        frame = 0;
        if (!sheet && !failed) return;
        const end = () => untrack(() => onend?.());
        if (!anim || matchMedia("(prefers-reduced-motion: reduce)").matches) {
            end();
            return;
        }
        const count = anim.frames.length;
        const loops = loop ?? anim.loop;
        const id = setInterval(() => {
            if (loops) frame = (frame + 1) % count;
            else if (frame < count - 1) frame++;
            else {
                clearInterval(id);
                end();
            }
        }, FRAME_MS);
        return () => clearInterval(id);
    });
</script>

{#if sheet}
    {@const f = (current ?? sheet.animations.idle).frames[frame] ?? sheet.animations.idle.frames[0]}
    <!-- Untouchable: the frame overflows the box, mostly transparent but
         still hit-tested, over whatever is above it (the back button). -->
    <svg
        viewBox="{f.x + sheet.crop.x} {f.y + sheet.crop.y} {sheet.crop.w} {sheet.crop.h}"
        width={sheet.crop.w}
        height={sheet.crop.h}
        overflow="visible"
        aria-hidden="true"
        class="pointer-events-none [image-rendering:pixelated] {className}"
    >
        <clipPath id={clipId}>
            <rect x={f.x} y={f.y} width={f.w} height={f.h} />
        </clipPath>
        <image
            href={sheetUrl(serial)}
            width={sheet.width}
            height={sheet.height}
            clip-path="url(#{clipId})"
        />
    </svg>
{:else if !failed}
    {#if still}
        <img
            src={bossStillUrl(serial)}
            alt=""
            width={still.w}
            height={still.h}
            draggable="false"
            hidden={stillHidden}
            onerror={() => (stillHidden = true)}
            class="pointer-events-none [image-rendering:pixelated] {className}"
        />
    {/if}
{:else}
    <img
        src="/{fallback}"
        alt=""
        width={BOSS_FALLBACKS[serial]?.w}
        height={BOSS_FALLBACKS[serial]?.h}
        draggable="false"
        {hidden}
        onerror={() => (hidden = true)}
        class="object-contain [image-rendering:pixelated] {className}"
    />
{/if}
