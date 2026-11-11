import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

export default defineConfig(({ mode }) => ({
	plugins: [sveltekit()],
	// Svelte 5 ships separate client/server builds selected via package.json
	// "exports" conditions. vitest runs in Node, so without forcing the
	// "browser" condition in test mode it resolves the server build, which
	// throws `lifecycle_function_unavailable` when @testing-library/svelte
	// calls mount() on component tests.
	resolve: {
		conditions: mode === 'test' ? ['browser'] : []
	},
	test: {
		environment: 'jsdom',
		include: ['src/**/*.test.ts', 'tests/**/*.test.ts'],
		// The real-Postgres integration suites (notifications.flow, retention.claims,
		// retention.tombstones) and the first test to import the AWS SDK graph
		// (storage.presign-endpoint) legitimately take 2-5s each; under a full parallel run —
		// especially a shuffled one, where they can all land in the same window — the default 5s
		// testTimeout flakes on whichever file loses the CPU lottery. Nothing here waits on a
		// condition, so a generous ceiling costs nothing on green runs.
		testTimeout: 20000,
		// Unmounts rendered components between tests — see vitest-setup.ts for why this is not
		// automatic here.
		setupFiles: ['./vitest-setup.ts']
	}
}));
