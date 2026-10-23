<script lang="ts">
    // Plays every boss's animations on demand, without a game: a top bar
    // like /play's, whose boss reacts the way /play's does (a hit plays
    // damage; a killing hit damage, then death, then the next boss), plus
    // every boss idling side by side to compare crops. Dev only.
    import BossSprite from "#lib/components/BossSprite.svelte";
    import DiamondBackdrop from "#lib/components/DiamondBackdrop.svelte";
    import scottFace from "#lib/assets/icons/scott-face.png";

    const BOSSES = [
        { serial: 1, name: "Matthew Patel" },
        { serial: 2, name: "Lucas Lee" },
        { serial: 3, name: "Todd Ingram" },
        { serial: 4, name: "Roxy Richter" },
        { serial: 5, name: "Ken & Kyle Katayanagi" },
        { serial: 6, name: "Gideon Graves" },
    ];
    const ANIMATIONS = ["idle", "attack", "damage", "death"];

    let serial = $state(1);
    // As on /play: hurt plays damage, dying holds the defeated boss until
    // its death has played, hits restarts a damage already playing.
    let dying = $state<number | null>(null);
    let hurt = $state(false);
    let hits = $state(0);
    // Any animation by name, played once (or looped, if the sheet says so)
    // until another button is pressed.
    let forced = $state<string | null>(null);
    let replays = $state(0);
    let health = $state(5);
    let healthDrops = $state(0);

    let shown = $derived(dying ?? serial);
    let animation = $derived(forced ?? (hurt ? "damage" : dying ? "death" : "idle"));
    let name = $derived(BOSSES.find((b) => b.serial === shown)?.name ?? `Boss ${shown}`);

    const pick = (s: number) => {
        forced = null;
        hurt = false;
        dying = null;
        serial = s;
    };
    const play = (anim: string) => {
        hurt = false;
        dying = null;
        forced = anim;
        replays++;
    };
    const hit = () => {
        if (dying) return;
        forced = null;
        hurt = true;
        hits++;
    };
    const kill = () => {
        if (dying) return;
        forced = null;
        dying = serial;
        serial = (serial % BOSSES.length) + 1;
        hurt = true;
        hits++;
    };
    const loseHealth = () => {
        health = health > 1 ? health - 1 : 5;
        healthDrops++;
    };
</script>

<svelte:head>
    <title>Boss animations</title>
</svelte:head>

<main class="min-h-dvh bg-night pb-8 font-pixel text-[8px] leading-[16px] text-white uppercase">
    <!-- The top bar, as on /play. -->
    <div class="relative isolate flex h-[25dvh] min-h-40 flex-col gap-2 overflow-hidden border-b-4 border-black px-4 py-3">
        <DiamondBackdrop class="absolute inset-0 -z-10" />
        <div class="flex shrink-0 items-center justify-end gap-2">
            {#key healthDrops}
                <p class="flex items-center gap-2" class:animate-blink-thrice={healthDrops > 0}>
                    <span class="text-[18px] leading-[32px] text-outline">x{health}</span>
                    <img src={scottFace} alt="" width="64" height="64" class="h-8 w-8 [image-rendering:pixelated]" />
                </p>
            {/key}
        </div>
        <div class="flex min-h-0 flex-1 items-center gap-4">
            {#key shown}
                <BossSprite
                    serial={shown}
                    fallback="bosses/{shown}.png"
                    {animation}
                    replay={forced ? replays : hits}
                    onend={() => {
                        if (forced) return;
                        if (hurt) hurt = false;
                        else dying = null;
                    }}
                    class="h-full w-auto shrink-0"
                />
            {/key}
            <div class="min-w-0">
                <p class="truncate text-[16px] leading-[24px] text-outline">{name}</p>
                <p class="text-outline">Playing: {animation}</p>
            </div>
        </div>
    </div>

    <section class="flex flex-col gap-6 px-4 pt-6">
        <div class="flex flex-col gap-3">
            <h2 class="text-gold-light">Boss</h2>
            <div class="flex flex-wrap gap-3">
                {#each BOSSES as b (b.serial)}
                    <button
                        class="pixel-box h-8 w-8 {b.serial === serial && !dying ? 'bg-pink' : 'bg-panel'}"
                        onclick={() => pick(b.serial)}
                        aria-label={b.name}>{b.serial}</button
                    >
                {/each}
            </div>
        </div>

        <div class="flex flex-col gap-3">
            <h2 class="text-gold-light">As in the game</h2>
            <div class="flex flex-wrap gap-3">
                <button class="pixel-box h-8 bg-gold px-3" onclick={hit}>Hit</button>
                <button class="pixel-box h-8 bg-danger px-3" onclick={kill}>Killing hit</button>
                <button class="pixel-box h-8 bg-panel px-3" onclick={loseHealth}>Lose health</button>
            </div>
        </div>

        <div class="flex flex-col gap-3">
            <h2 class="text-gold-light">One animation</h2>
            <div class="flex flex-wrap gap-3">
                {#each ANIMATIONS as anim (anim)}
                    <button
                        class="pixel-box h-8 px-3 {forced === anim ? 'bg-pink' : 'bg-panel'}"
                        onclick={() => play(anim)}>{anim}</button
                    >
                {/each}
            </div>
        </div>

        <div class="flex flex-col gap-3">
            <h2 class="text-gold-light">All bosses, idle</h2>
            <div class="grid grid-cols-3 gap-4">
                {#each BOSSES as b (b.serial)}
                    <figure class="flex flex-col items-center gap-2 bg-ink/60 p-2">
                        <BossSprite serial={b.serial} fallback="bosses/{b.serial}.png" class="h-24 w-auto" />
                        <figcaption class="text-center">{b.serial}</figcaption>
                    </figure>
                {/each}
            </div>
        </div>
    </section>
</main>
