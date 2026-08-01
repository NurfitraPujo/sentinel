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
		// Unmounts rendered components between tests — see vitest-setup.ts for why this is not
		// automatic here.
		setupFiles: ['./vitest-setup.ts']
	}
}));
