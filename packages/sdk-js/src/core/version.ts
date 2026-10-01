// `SDK_VERSION` (spec §3.3): a string literal, not a build-time define, so the vitest suite sees the same value
// as `dist` and `tests/package.test.ts` can assert it equals `package.json`'s `version` directly. Bump both
// together — C11 does, at the release commit.

export const SDK_VERSION = '0.3.0';
