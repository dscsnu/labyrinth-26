<script lang="ts">
    import { BACKEND_URL } from "$app/env/public";
    import DiamondBackdrop from "#lib/components/DiamondBackdrop.svelte";
    import scottFace from "#lib/assets/icons/scott-face.png";
    import type { PageProps } from "./$types";

    let { data }: PageProps = $props();
    let { user } = $derived(data);

    // A full-page navigation, not fetch: the backend redirects to Google.
    const loginUrl = `${BACKEND_URL.replace(/\/+$/, "")}/auth/google/login?next=${encodeURIComponent("/")}`;

    // The title screen's menu, by role. Signing out is the hint at the bottom.
    let items = $derived(
        user
            ? [
                  { label: "Team", href: "/team" },
                  { label: "Test scan", href: "/test-scan" },
                  ...(user.role === "HELPER" || user.role === "ADMIN" ? [{ label: "QR display", href: "/qr" }] : []),
                  ...(user.role === "ADMIN" ? [{ label: "Admin", href: "/admin" }] : []),
              ]
            : [{ label: "Sign in with Google", href: loginUrl }],
    );

    // The selected item sits on the black band, like the game's menu: moved
    // by hover, focus or the arrow keys. Tapping an item opens it.
    let selected = $state(0);
    let links: HTMLAnchorElement[] = $state([]);
    const BAND_SIDE_REPEATS = 3;

    const keydown = (e: KeyboardEvent) => {
        if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
        e.preventDefault();
        selected = (selected + (e.key === "ArrowDown" ? 1 : -1) + items.length) % items.length;
        links[selected]?.focus();
    };
</script>

<svelte:head><title>Labyrinth</title></svelte:head>

<div class="relative isolate flex min-h-dvh flex-col overflow-x-hidden pb-36 font-pixel text-[16px] leading-[24px] text-white">
    <DiamondBackdrop class="fixed inset-0 -z-10" />

    <main class="flex flex-1 flex-col justify-center gap-10 py-12">
        <!-- The logo, Scott above it -->
        <header class="flex flex-col items-center px-5 text-center">
            <img src={scottFace} alt="" width="64" height="64" draggable="false" class="h-32 w-32 [image-rendering:pixelated]" />
            <h1 class="mt-4 font-display text-[clamp(40px,14vw,96px)] leading-[1] text-cyan uppercase logo-outline">Labyrinth</h1>
            <p class="mt-6 uppercase text-outline">vs. the <span class="text-pink">seven evil exes</span></p>
            {#if user}
                <p class="mt-3 text-[8px] leading-[16px] uppercase text-outline">Player: {user.name}</p>
            {/if}
        </header>

        <!-- The menu: the selected item on a full-width black band, its label
             repeated faded either side. -->
        <nav aria-label="Main menu">
            <ul class="flex flex-col gap-1">
                {#each items as item, i (item.href)}
                    {@const on = i === selected}
                    <li class="flex justify-center gap-10 overflow-hidden whitespace-nowrap uppercase {on ? 'bg-black py-1' : 'py-1'}">
                        {#if on}
                            {#each { length: BAND_SIDE_REPEATS }, r (r)}
                                <span aria-hidden="true" class="shrink-0 text-tile-light">{item.label}</span>
                            {/each}
                        {/if}
                        <a
                            bind:this={links[i]}
                            href={item.href}
                            onmouseenter={() => (selected = i)}
                            onfocus={() => (selected = i)}
                            onkeydown={keydown}
                            class="shrink-0 px-1 outline-none {on ? 'bg-pink' : 'text-outline'}"
                        >
                            {item.label}
                        </a>
                        {#if on}
                            {#each { length: BAND_SIDE_REPEATS }, r (r)}
                                <span aria-hidden="true" class="shrink-0 text-tile-light">{item.label}</span>
                            {/each}
                        {/if}
                    </li>
                {/each}
            </ul>
        </nav>
    </main>

    <!-- The bottom bar: sign out on the left (like /team's back), and under
         it, centred, a note that this look is borrowed for now. -->
    <footer class="fixed inset-x-0 bottom-0 flex flex-col gap-3 bg-black/60 px-4 py-3 pb-[max(0.75rem,env(safe-area-inset-bottom))]">
        {#if user}
            <form method="POST" action="/auth/logout" class="self-start">
                <button type="submit" class="flex items-center gap-2 uppercase text-outline hover:text-pink">
                    <span aria-hidden="true" class="flex h-6 w-6 items-center justify-center rounded-full border-2 border-ink bg-[#c7ccd8]">
                        <span class="h-3 w-3 rounded-full border-2 border-danger"></span>
                    </span>
                    Log out
                </button>
            </form>
        {/if}
        <p class="mx-auto max-w-md text-center text-[8px] leading-[16px] text-white/80">
            Don't mind me copying the game ui shamelessly, i will change this up with our assets later once the complete
            integration is done
        </p>
    </footer>
</div>
