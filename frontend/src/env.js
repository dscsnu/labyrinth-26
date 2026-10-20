import { defineEnvVars } from '@sveltejs/kit/env';

export const variables = defineEnvVars({
    BACKEND_URL: {
        public: true,
        description: 'Base URL of the Go backend, reachable from both the browser and the SvelteKit server',
    },
    JWT_PUBLIC_KEY: {
        description: "Base64 SPKI Ed25519 public key matching the backend's JWT_PRIVATE_KEY (print it with `go run . pubkey` in backend/)",
    },
    DEV_TOKEN: {
        description: 'Dev only: a labyrinth_token JWT used when the request has no session cookie, so devices that cannot sign in (e.g. a phone on the LAN) see signed-in pages',
        schema: (value) => value || undefined,
    },
});
