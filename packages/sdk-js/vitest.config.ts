import { defineConfig } from 'vitest/config';

// Every suite here is written shuffle-safe (contract §4.1, panel round 5): run
// `pnpm test -- --sequence.shuffle` before trusting the suite.
export default defineConfig({
  test: {
    include: ['tests/**/*.test.ts'],
    environment: 'node',
  },
});
