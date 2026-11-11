import { env } from '$env/dynamic/private';

/**
 * sentinel-affected-users §3 / decision 21 — the retention window, in days, as ONE expression.
 *
 * `DATA_RETENTION_DAYS` was read in exactly one place, `routes/api/cron/retention/+server.ts`, and
 * the sweep is what gives the number its meaning: an affected-user count is "distinct ids in the
 * last N days" (decision 12) because the sweep removes side rows older than N days. Every label
 * that carries a count carries the window, and a Svelte template cannot read
 * `$env/dynamic/private`, so both page loaders (`/`, `/issues/{id}`) put `retentionDays` on page
 * data — and they, and the cron route, must agree on N by construction, not by three copies of
 * `parseInt(env.DATA_RETENTION_DAYS ?? '30', 10)` that can drift.
 *
 * Lives under `$lib/server/` and not in a route file: `+page.server.ts` / `+server.ts` may export
 * only SvelteKit's allowlisted names, and `pnpm build` is the only gate that enforces it (B12).
 * Read on every call, not at module load, because `$env/dynamic/private` is dynamic and the cron
 * route reads it per request today.
 */
export function getRetentionDays(): number {
	return parseInt(env.DATA_RETENTION_DAYS ?? '30', 10);
}
