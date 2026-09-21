import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vitest/config';
import type { ProxyOptions } from 'vite';
import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';

// Development proxy: relayd enforces the exact canonical Host and
// Origin, so the proxy deliberately rewrites both to the configured
// target. This relaxation exists only inside the local Vite proxy —
// relayd itself has no dev-origin bypass. RELAY_DEV_TARGET is the
// daemon's printed endpoint, e.g. http://127.0.0.1:<port>.
const relayTarget = process.env.RELAY_DEV_TARGET;

function relayProxy(target: string): ProxyOptions {
	const url = new URL(target);
	return {
		target,
		changeOrigin: true,
		headers: {
			// Overwrite the browser's Origin/Host so relayd sees exactly the
			// canonical loopback authority it enforces.
			host: url.host,
			origin: url.origin
		}
	};
}

export default defineConfig({
	plugins: [
		tailwindcss(),
		sveltekit({
			compilerOptions: {
				// Force runes mode for the project, except for libraries. Can be removed in svelte 6.
				runes: ({ filename }) => filename.split(/[/\\]/).includes('node_modules') ? undefined : true
			},
			adapter: adapter({
				fallback: 'index.html',
				strict: false
			}),
			version: {
				// Deterministic build: the default timestamp version would
				// cascade into chunk hashes and defeat the committed-build
				// drift gate. A fixed name makes every build reproducible.
				name: '0.1.0',
				pollInterval: 0
			}
		})
	],
	server:
		relayTarget === undefined
			? {}
			: {
					proxy: {
						'/auth': relayProxy(relayTarget),
						'/v1': relayProxy(relayTarget)
					}
				},
	test: {
		expect: { requireAssertions: true },
		projects: [
			{
				extends: './vite.config.ts',
				test: {
					name: 'server',
					environment: 'node',
					include: ['src/**/*.{test,spec}.{js,ts}'],
					exclude: ['src/**/*.svelte.{test,spec}.{js,ts}']
				}
			}
		]
	}
});
