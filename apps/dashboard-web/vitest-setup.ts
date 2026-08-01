import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/svelte';

// @testing-library/svelte auto-registers this cleanup ONLY when vitest runs with `globals: true`,
// which this project does not set. Without it, a component rendered in one test stays mounted in
// the shared jsdom document for the rest of the file, so later queries match elements left over
// from earlier tests — e.g. `getByRole('button', { name: /unlink issue/i })` throwing "Found
// multiple elements". That made the component suites order-dependent: green in declaration order,
// failing under `vitest --sequence.shuffle`.
//
// Registered here rather than per-file so tests added later inherit it by default.
afterEach(() => {
	cleanup();
});
