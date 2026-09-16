// `@sentinel/sdk-js/nextjs/server` (contract §4.1, decision 31): a fixed typed entry with its own `.d.ts` and no
// export conditions — the `server` init file imports this string, never the conditional `./nextjs`.
//
// Scaffold (sentinel-sdk-js/TASK-01): the Next.js adapter lands in task 6 (contract §4.7).
// Import-time inertness (contract §11): the module body registers nothing until `init`.

export { SDK_NAME } from '../core/index.js';
