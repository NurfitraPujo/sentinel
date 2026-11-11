import { env } from '$env/dynamic/private';

/**
 * sentinel-reporting-and-dashboard D4/D10/D14: the Databend client.
 *
 * Chosen: Databend's HTTP query API (`POST /v1/query`, then follow `next_uri`) over `fetch`, with validated literals
 * only (literals.ts). Why not the `databend-driver` npm package: it is a native (napi) addon with per-platform
 * prebuilt binaries; the dashboard image is built and run on more than one platform (linux arm64 on the EKS t4g
 * nodes, amd64 in CI and on developer machines), so a package that must build or fetch a binary per platform is a
 * build risk for one query path. The HTTP API is plain JSON, has no install-time dependency, and is what the
 * contract names (`databend-query.databend.svc.cluster.local:8000`, "HTTP query API").
 *
 * Failure isolation (D14): every query has a 10 s timeout. A missing configuration, a refusal or a timeout is a
 * `DatabendUnavailableError`; the caller turns it into "unavailable" for that one section and nothing else changes.
 */

export const QUERY_TIMEOUT_MS = 10_000;

export class DatabendUnavailableError extends Error {}

/** Databend returns every cell as a string (or null). */
export type Row = (string | null)[];
export type Run = (sql: string) => Promise<Row[]>;

export interface DatabendConfig {
	url: string;
	user: string;
	password: string;
	database: string;
}

export function databendConfigFromEnv(): DatabendConfig | null {
	if (!env.DATABEND_URL) return null;
	return {
		url: env.DATABEND_URL,
		user: env.DATABEND_USER ?? 'sentinel_app',
		password: env.DATABEND_PASSWORD ?? '',
		database: env.DATABEND_DATABASE ?? 'sentinel',
	};
}

export function createRun(cfg: DatabendConfig | null, fetchFn: typeof fetch = fetch, timeoutMs = QUERY_TIMEOUT_MS): Run {
	return async (sql) => {
		if (!cfg) throw new DatabendUnavailableError('Databend is not configured');
		const deadline = AbortSignal.timeout(timeoutMs);
		const auth = 'Basic ' + Buffer.from(`${cfg.user}:${cfg.password}`).toString('base64');
		const headers = { 'Content-Type': 'application/json', Authorization: auth };
		const base = cfg.url.replace(/\/+$/, '');
		const rows: Row[] = [];
		try {
			let res = await fetchFn(`${base}/v1/query/`, {
				method: 'POST',
				headers,
				signal: deadline,
				body: JSON.stringify({ sql, session: { database: cfg.database }, pagination: { wait_time_secs: 5 } }),
			});
			for (let hops = 0; hops < 1000; hops++) {
				if (!res.ok) throw new DatabendUnavailableError(`Databend answered ${res.status}`);
				const body = (await res.json()) as {
					error?: { message?: string } | null;
					data?: Row[];
					next_uri?: string | null;
					state?: string;
				};
				if (body.error) throw new DatabendUnavailableError(`Databend query failed: ${String(body.error.message).slice(0, 300)}`);
				if (body.data) rows.push(...body.data);
				if (!body.next_uri) return rows;
				res = await fetchFn(`${base}${body.next_uri}`, { headers, signal: deadline });
			}
			throw new DatabendUnavailableError('Databend paging did not finish');
		} catch (err) {
			if (err instanceof DatabendUnavailableError) throw err;
			throw new DatabendUnavailableError(`Databend request failed: ${(err as Error).message}`);
		}
	};
}
