<script lang="ts">
	import "#lib/styles/tailwind.css";
	import favicon from "#lib/assets/favicon.svg";
	import type { LayoutProps } from "./$types";
	import { onMount } from "svelte";
	import { setDevice } from "#lib/stores/DeviceStore.ts";

	let { children }: LayoutProps = $props();

	onMount(() => {
		if (window.matchMedia("(max-width: 767px)").matches)
			setDevice("mobile");
		else setDevice("desktop");
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
