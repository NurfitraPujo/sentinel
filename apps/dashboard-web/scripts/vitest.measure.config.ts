import { mergeConfig } from 'vitest/config';
import base from '../vite.config.js';

/**
 * The config for the `scripts/*.measure.ts` drivers — measurement scripts that call the real
 * readers (`issueQueries.listIssues`, `trendForIssues`, `trendForIssue`) against a seeded
 * DISPOSABLE Postgres and print the tables `status/be.md` pastes. They are not tests: `pnpm test`'s
 * `include` (`src/**\/*.test.ts`, `tests/**\/*.test.ts`) never sees them, CI never runs them, and
 * nothing here edits `vite.config.js`. The SvelteKit plugin is kept so `$lib` and `$env` resolve
 * exactly as the loaders see them; jsdom and the component-cleanup setup file are dropped because
 * nothing here renders.
 *
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15806/sentinel \
 *     pnpm vitest run --config scripts/vitest.measure.config.ts
 */
export default (env: { mode: string; command: 'build' | 'serve' }) => {
	const merged = mergeConfig(base(env), {
		test: {
			environment: 'node',
			setupFiles: [] as string[],
			// a measurement loop of ≥ 30 runs × several statements; generous so the driver never
			// reports a timeout as a measurement
			testTimeout: 120_000
		}
	});
	// mergeConfig concatenates arrays, and the base `include` must NOT survive here: the drivers
	// are the only files this config runs.
	merged.test.include = ['scripts/**/*.measure.ts'];
	merged.test.setupFiles = [];
	return merged;
};
