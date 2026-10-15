<script lang="ts">
	import "#lib/styles/tailwind.css";
	import favicon from "#lib/assets/favicon.svg";
	import type { LayoutProps } from "./$types";
	import { onMount } from "svelte";
	import { refreshAll } from "$app/navigation";
	import { SupaStore, UserStore } from "#lib/stores/SupabaseStore.ts";
	import { setDevice } from "#lib/stores/DeviceStore.ts";

	let { data, children }: LayoutProps = $props();
	let { supabase, session, user } = $derived(data);

	onMount(() => {
		const {
			data: { subscription },
		} = supabase.auth.onAuthStateChange((event, newSession) => {
			if (
				newSession?.expires_at !== session?.expires_at ||
				event === "SIGNED_OUT"
			) {
				refreshAll();
			}
		});

		SupaStore.set(supabase);
		UserStore.set(user);

		if (window.matchMedia("(max-width: 767px)").matches)
			setDevice("mobile");
		else setDevice("desktop");

		return () => subscription.unsubscribe();
	});
</script>

<svelte:head>
	<link rel="icon" href={favicon} />
</svelte:head>

<!-- handle device resize -->
<svelte:window
	onresize={() =>
		setDevice(
			window.matchMedia("(max-width: 767px").matches
				? "mobile"
				: "desktop",
		)}
/>

{@render children()}
