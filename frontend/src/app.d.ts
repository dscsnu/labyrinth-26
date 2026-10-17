// See https://svelte.dev/docs/kit/types#app.d.ts

import type { SessionUser } from "#lib/types/User.type.ts";

// for information about these interfaces
declare global {
	namespace App {
		// interface Error {}
		interface Locals {
			user: SessionUser | null;
			// backend JWT, for server-side calls to the backend on the user's behalf
			token: string | null;
		}
		interface PageData {
			user: SessionUser | null;
		}
		// interface PageState {}
		// interface Platform {}
	}
	type Device = 'mobile' | 'desktop';
}

export { };
