// `@sentinel/sdk-js/nextjs` — the types of the conditional runtime entry (contract §4.1, decision 31).
//
// This file is hand-written and copied to dist/nextjs/index.d.ts by scripts/build.mjs; it is the `types`
// condition of `./nextjs`. It declares ONLY the runtime-agnostic re-exports — never `init` — so
// `instrumentation.ts` and code bundled for more than one runtime compile in every bundle, and
// `import { init } from '@sentinel/sdk-js/nextjs'` stays a type error (flows B18).
//
// Scaffold (sentinel-sdk-js/TASK-01): the re-exports are declared in task 6 (contract §4.7).

export { SDK_NAME } from '../core/index.js';
