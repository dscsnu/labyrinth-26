<script lang="ts">
    import QRCode from 'qrcode';
    import { SigningKeySchema, type SigningKey } from '#lib/types/Qr.type.ts';
    import { onMount } from 'svelte';
    import type { PageProps } from './$types';

    let { data }: PageProps = $props();
    let { user } = $derived(data);

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
        return () => {
            if (tick) clearInterval(tick);
            if (rotator) clearInterval(rotator);
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

    const start = async (e: SubmitEvent) => {
        e.preventDefault();
        const id = waypoint.trim();
        if (!/^[0-9]{4}$/.test(id)) return (message = { ok: false, text: 'waypoint id must be 4 digits' });

        const res = await fetch(`/qr/${encodeURIComponent(id)}`);
        const json = await res.json().catch(() => ({}));
        if (!res.ok) return (message = { ok: false, text: `${json.code}: ${json.message}` });

        try {
            info = SigningKeySchema.parse(json.payload);
        } catch {
            return (message = { ok: false, text: 'unexpected response from the backend; is it the current build?' });
        }
        message = null;
        offset = info.server_time - Math.floor(Date.now() / 1000);
        hmacKey = await importKey(info.key);

        if (rotator) clearInterval(rotator);
        await renderNext();
        rotator = setInterval(renderNext, Math.max(500, info.rotate_ms));
    };

    $effect(() => {
        if (canvas && tokenStr) QRCode.toCanvas(canvas, tokenStr, { width: 320, margin: 2 });
    });

    let secondsLeft = $derived(
        info ? Math.max(0, Math.round(issuedSecs + info.window_ms / 1000 - (now / 1000 + offset))) : 0,
    );
</script>

<p><a href="/">home</a> · {user?.name} · qr display</p>

<h1>QR display</h1>
<p><small>Enter a waypoint's 4-digit ID to show its rotating QR.</small></p>

<form onsubmit={start}>
    <input bind:value={waypoint} inputmode="numeric" maxlength="4" placeholder="2001" />
    <button>Show</button>
</form>

{#if info}
    <p>Waypoint <b>{info.waypoint_id}</b> · rotates every {info.rotate_ms / 1000} s · {secondsLeft} s left</p>
    <canvas bind:this={canvas}></canvas>
{/if}

{#if message}
    <p style:color={message.ok ? "green" : "red"}>{message.text}</p>
{/if}
