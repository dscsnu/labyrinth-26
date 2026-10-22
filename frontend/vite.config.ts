import tailwindcss from '@tailwindcss/vite';
import adapter from '@sveltejs/adapter-node';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig, loadEnv } from 'vite';

export default defineConfig(({ mode }) => {
	const env = loadEnv(mode, process.cwd(), '');
	return {
		plugins: [
			tailwindcss(),
			sveltekit({
				compilerOptions: {
					// Force runes mode for the project, except for libraries. Can be removed in svelte 6.
					runes: ({ filename }) =>
						filename.split(/[/\\]/).includes('node_modules') ? undefined : true
				},

				// A Node server in build/ (node build), run in production by the
				// frontend image (Dockerfile). Static assets are precompressed
				// (gzip, brotli), and /_app/immutable is served cacheable forever.
				adapter: adapter()
			})
		],
		server: {
			// The browser opens the event stream at /api/events on this origin,
			// with the labyrinth_token cookie (EventSource can't send headers).
			// In production Caddy routes it to the backend (caddy/Caddyfile);
			// in dev, this does.
			proxy: {
				'/api/events': { target: env.BACKEND_URL, changeOrigin: true }
			}
		}
	};
});
