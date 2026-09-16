import { defineConfig } from 'vitest/config';

// `pnpm bench` (contract §13): a LOCAL vitest bench, never a CI step — nothing in ci.yml runs it and no reviewer
// should look for an artefact. Kept out of the test config's `include` so `pnpm test` never picks it up.
export default defineConfig({
  test: {
    include: [],
    benchmark: {
      include: ['bench/**/*.bench.ts'],
    },
    environment: 'node',
  },
});
