<script lang="ts">
    import jsQR from 'jsqr';
    import { QRVerifySchema } from '#lib/types/Qr.type.ts';
    import { onMount } from 'svelte';
    import type { PageProps } from './$types';

    let { data }: PageProps = $props();
    let { user } = $derived(data);

    let video = $state<HTMLVideoElement>();
    let stream = $state<MediaStream | null>(null);
    let result = $state<{ ok: boolean; text: string } | null>(null);
    let busy = $state(false);

    let loop: ReturnType<typeof setInterval> | null = null;

    const decode = () => {
        if (!video || busy) return;
        const w = video.videoWidth;
        const h = video.videoHeight;
        if (!w || !h) return;
        const c = document.createElement('canvas');
        c.width = w;
        c.height = h;
        const ctx = c.getContext('2d');
        if (!ctx) return;
        ctx.drawImage(video, 0, 0, w, h);
        const code = jsQR(ctx.getImageData(0, 0, w, h).data, w, h);
        if (code?.data) void submit(code.data);
    };

    const submit = async (qr: string) => {
        busy = true;
        const res = await fetch('/qr/verify', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ qr }),
        });
        const json = await res.json().catch(() => ({}));
        result = res.ok
            ? { ok: true, text: `waypoint ${QRVerifySchema.parse(json.payload).waypoint_id}` }
            : { ok: false, text: `${json.code}: ${json.message}` };
        busy = false;
    };

    onMount(() => {
        (async () => {
            try {
                stream = await navigator.mediaDevices.getUserMedia({ video: { facingMode: 'environment' } });
                if (video) {
                    video.srcObject = stream;
                    await video.play();
                }
                loop = setInterval(decode, 250);
            } catch (e) {
                result = { ok: false, text: `camera unavailable: ${e}` };
            }
        })();
        return () => {
            if (loop) clearInterval(loop);
            stream?.getTracks().forEach((t) => t.stop());
        };
    });
</script>

<p><a href="/">home</a> · <a href="/qr">qr display</a> · {user?.name} · test-scan</p>

<h1>Test scan</h1>
<p><small>Point the camera at a /qr display's rotating code. The backend verifies the token.</small></p>

<video bind:this={video} autoplay playsinline muted></video>

{#if result}
    <p style:color={result.ok ? "green" : "red"}>{result.text}</p>
{/if}
