<script lang="ts">
    // A boss, animated from static/bosses_animation/<serial>.png, the
    // spritesheet its <serial>.json lays out: animation loops if the sheet
    // says so (or loop does), else plays once, holds its last frame and calls onend; a
    // change to replay plays it again from the start.
    // Without a sheet (missing or malformed JSON) it falls back to the still
    // static/<fallback>, hidden if that's missing too; then, as without the
    // animation or with reduced motion, onend is called at once.
    import { untrack } from "svelte";
    import { z } from "zod";

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

    // The JSON has no timing.
    const FRAME_MS = 125;

    type Sheet = {
        width: number;
        height: number;
        animations: Record<string, { loop: boolean; frames: z.infer<typeof FrameSchema>[] }>;
        // One box, relative to a frame's left edge, round every idle frame's
        // sprite: frames are mostly empty above the boss, and sharing a box
        // keeps its baseline still. Every animation is drawn at its scale;
        // bigger ones (death's beam) spill out of it, clipped to their frame.
        crop: { x: number; y: number; w: number; h: number };
    };

    const clipId = $props.id();
    let sheet = $state<Sheet | null>(null);
    let failed = $state(false);
    let frame = $state(0);
    let hidden = $state(false);

    $effect(() => {
        const s = serial;
        sheet = null;
        failed = false;
        let cancelled = false;
        fetch(`/bosses_animation/${s}.json`)
            .then((res) => (res.ok ? res.json() : Promise.reject(res.status)))
            .then((json) => {
                if (cancelled) return;
                const { imageWidth, imageHeight, animations } = SheetSchema.parse(json);
                const idle = animations.idle.frames;
                const left = Math.min(...idle.map((f) => f.sprite.x - f.x));
                const top = Math.min(...idle.map((f) => f.sprite.y));
                const right = Math.max(...idle.map((f) => f.sprite.x - f.x + f.sprite.w));
                const bottom = Math.max(...idle.map((f) => f.sprite.y + f.sprite.h));
                sheet = {
                    width: imageWidth,
                    height: imageHeight,
                    animations,
                    crop: { x: left, y: top, w: right - left, h: bottom - top },
                };
            })
            .catch(() => {
                if (!cancelled) failed = true;
            });
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
        viewBox="{f.x + sheet.crop.x} {sheet.crop.y} {sheet.crop.w} {sheet.crop.h}"
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
            href="/bosses_animation/{serial}.png"
            width={sheet.width}
            height={sheet.height}
            clip-path="url(#{clipId})"
        />
    </svg>
{:else if failed}
    <img
        src="/{fallback}"
        alt=""
        width="628"
        height="802"
        draggable="false"
        {hidden}
        onerror={() => (hidden = true)}
        class="object-contain {className}"
    />
{/if}
