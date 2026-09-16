// The router-transition hook (contract §4.7, flows B20): Next calls `instrumentation-client.ts`'s exported
// `onRouterTransitionStart(url, navigationType)` on every client navigation; this SDK records no navigation
// spans (decision D7), so the hook is a no-op — exported under both names so `web`'s
// `export const onRouterTransitionStart = Sentry.captureRouterTransitionStart` compiles with the import swapped.
// Provided by every build (the union `./nextjs` types declare it), so a utility bundled for more than one
// runtime resolves it everywhere.

export type NavigationType = 'push' | 'replace' | 'traverse';

/** Next's `onRouterTransitionStart` signature (`next/dist/client/app-index.d.ts` at 15.5.21); does nothing. */
export function onRouterTransitionStart(_url: string, _navigationType: NavigationType): void {
  // No navigation tracing (decision D7).
}

export const captureRouterTransitionStart: (url: string, navigationType: NavigationType) => void = onRouterTransitionStart;
