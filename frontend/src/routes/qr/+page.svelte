<script lang="ts">
    import QRCode from 'qrcode';
    import { SigningKeySchema, type SigningKey } from '#lib/types/Qr.type.ts';
    import { MAP_POINTS } from '#lib/game/mapPoints.ts';
    import { onMount } from 'svelte';
    import type { PageProps } from './$types';

    let { data }: PageProps = $props();
    let { user, role } = $derived(data);
    // An admin picks any waypoint; a helper shows only the checkpoint an
    // admin assigned them (GET /qr/me), re-checked while the page is open.
    let isAdmin = $derived(role === 'ADMIN');
    // Unassigned: no checkpoint yet, so there's nothing to show.
    let unassigned = $state(false);

    // How often a helper's page asks for its assignment: soon while it has
    // none, less often once it's showing one.
    const RECHECK_UNASSIGNED_MS = 15_000;
    const RECHECK_ASSIGNED_MS = 30_000;
    let recheck: ReturnType<typeof setTimeout> | null = null;

    let waypoint = $state('');
    let info = $state<SigningKey | null>(null);
    let message = $state<{ ok: boolean; text: string } | null>(null);
    let canvas = $state<HTMLCanvasElement>();
    let now = $state(Date.now());

    let issuedSecs = $state(0);
    let tokenStr = $state('');
    let offset = 0;
    let hmacKey: CryptoKey | null = null;

    let tick: ReturnType<typeof setInterval> | null = null;
    let rotator: ReturnType<typeof setInterval> | null = null;

    onMount(() => {
        tick = setInterval(() => (now = Date.now()), 500);
        if (!isAdmin) void checkAssignment();
        return () => {
            if (tick) clearInterval(tick);
            if (rotator) clearInterval(rotator);
            if (recheck) clearTimeout(recheck);
        };
    });

    const hexToBytes = (hex: string): Uint8Array<ArrayBuffer> => {
        const out = new Uint8Array(hex.length / 2);
        for (let i = 0; i < out.length; i++) out[i] = parseInt(hex.slice(i * 2, i * 2 + 2), 16);
        return out;
    };

    const b64url = (bytes: Uint8Array): string => {
        let bin = '';
        for (const b of bytes) bin += String.fromCharCode(b);
        return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
    };

    // Each waypoint has its own key, so it's imported on every show: a key
    // kept from the last waypoint would sign tokens the backend rejects.
    const importKey = (hex: string): Promise<CryptoKey> =>
        crypto.subtle.importKey('raw', hexToBytes(hex), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);

    const sign = async (key: CryptoKey, payload: string): Promise<string> => {
        const sig = await crypto.subtle.sign('HMAC', key, new TextEncoder().encode(payload));
        return b64url(new Uint8Array(sig));
    };

    // Sign a fresh token for the current second and re-render the QR.
    const renderNext = async () => {
        if (!info || !hmacKey) return;
        const secs = Math.floor(Date.now() / 1000) + offset;
        const payload = `${info.waypoint_id}:${secs}`;
        issuedSecs = secs;
        tokenStr = `${payload}.${await sign(hmacKey, payload)}`;
    };

    // Starts showing a waypoint from its signing key (a GET /qr/… payload).
    const show = async (payload: unknown) => {
        let next: SigningKey;
        try {
            next = SigningKeySchema.parse(payload);
        } catch {
            message = { ok: false, text: 'unexpected response from the backend; is it the current build?' };
            return;
        }
        message = null;
        offset = next.server_time - Math.floor(Date.now() / 1000);
        hmacKey = await importKey(next.key);
        info = next;

        if (rotator) clearInterval(rotator);
        await renderNext();
        rotator = setInterval(renderNext, Math.max(500, next.rotate_ms));
    };

    // Stops showing a QR (a helper unassigned).
    const hide = () => {
        if (rotator) clearInterval(rotator);
        rotator = null;
        info = null;
        hmacKey = null;
        tokenStr = '';
    };

    // An admin's picker: any waypoint by its 4-digit ID.
    const start = async (e: SubmitEvent) => {
        e.preventDefault();
        const id = waypoint.trim();
        if (!/^[0-9]{4}$/.test(id)) return (message = { ok: false, text: 'waypoint id must be 4 digits' });

        const res = await fetch(`/qr/${encodeURIComponent(id)}`);
        const json = await res.json().catch(() => ({}));
        if (!res.ok) return (message = { ok: false, text: `${json.code}: ${json.message}` });
        await show(json.payload);
    };

    // A helper's checkpoint: shown when assigned, switched when it changes,
    // hidden when it's taken away. A failed check (no signal) keeps the QR
    // showing: it signs locally, so it keeps working offline.
    const checkAssignment = async () => {
        const res = await fetch('/qr/me').catch(() => null);
        const json = res ? await res.json().catch(() => ({})) : null;
        if (res?.ok) {
            unassigned = false;
            if (json.payload?.waypoint_id !== info?.waypoint_id) await show(json.payload);
        } else if (json?.code === 'not_assigned') {
            unassigned = true;
            message = null;
            hide();
        } else if (json?.code === 'not_staff') {
            hide();
            message = { ok: false, text: "You're no longer a helper." };
            return;
        } else if (!info) {
            message = { ok: false, text: res ? `${json?.code}: ${json?.message}` : 'No connection: retrying.' };
        }
        recheck = setTimeout(checkAssignment, info ? RECHECK_ASSIGNED_MS : RECHECK_UNASSIGNED_MS);
    };

    // The waypoint's place name on the map, if it has one.
    const placeName = (id: string) => MAP_POINTS.find((p) => p.waypoint === id)?.name;

    $effect(() => {
        if (canvas && tokenStr) QRCode.toCanvas(canvas, tokenStr, { width: 320, margin: 2 });
    });

    let secondsLeft = $derived(
        info ? Math.max(0, Math.round(issuedSecs + info.window_ms / 1000 - (now / 1000 + offset))) : 0,
    );
</script>

<p><a href="/">home</a> · {user?.name} · qr display</p>

<h1>QR display</h1>
{#if isAdmin}
    <p><small>Enter a waypoint's 4-digit ID to show its rotating QR.</small></p>

    <form onsubmit={start}>
        <input bind:value={waypoint} inputmode="numeric" maxlength="4" placeholder="2001" />
        <button>Show</button>
    </form>
{:else if unassigned}
    <p>No checkpoint assigned yet: ask an organiser. This page checks again by itself.</p>
{/if}

{#if info}
    <p>
        Checkpoint <b>{placeName(info.waypoint_id) ?? info.waypoint_id}</b> ({info.waypoint_id}) · rotates every
        {info.rotate_ms / 1000} s · {secondsLeft} s left
    </p>
    <canvas bind:this={canvas}></canvas>
{/if}

{#if message}
    <p style:color={message.ok ? "green" : "red"}>{message.text}</p>
{/if}
