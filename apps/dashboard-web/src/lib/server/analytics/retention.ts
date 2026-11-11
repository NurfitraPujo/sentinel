import { DatabendUnavailableError, type Run } from './databend';
import { tsLiteral } from './literals';

/**
 * sentinel-reporting-and-dashboard D5: retention in Databend is 13 months.
 *
 * The existing hourly retention run also deletes, in Databend:
 *   - `error_occurrences` by `created_at`;
 *   - `issue_activity` by `created_at`;
 *   - soft-deleted `issues` (the CDC sink turns a Postgres delete into `deleted_at`, D3) by `last_seen`.
 * `sli_hourly` is kept forever (a few rows an hour). Cutoffs are server-generated timestamps, never request input.
 *
 * Isolation: this NEVER throws. A Databend failure is returned as `failed` (the route logs and counts it) and the
 * Postgres retention run's outcome is untouched.
 */
export const DATABEND_RETENTION_MONTHS = 13;

export interface DatabendRetentionResult {
	outcome: 'completed' | 'failed';
	cutoff: Date;
	error?: string;
}

/** 13 months back, calendar-aware (UTC). */
export function databendCutoff(now: Date): Date {
	const d = new Date(now.getTime());
	d.setUTCMonth(d.getUTCMonth() - DATABEND_RETENTION_MONTHS);
	return d;
}

export async function purgeDatabend(run: Run, now: Date = new Date()): Promise<DatabendRetentionResult> {
	const cutoff = databendCutoff(now);
	const at = tsLiteral(cutoff);
	try {
		await run(`DELETE FROM error_occurrences WHERE created_at < ${at}`);
		await run(`DELETE FROM issue_activity WHERE created_at < ${at}`);
		await run(`DELETE FROM issues WHERE deleted_at IS NOT NULL AND last_seen < ${at}`);
		return { outcome: 'completed', cutoff };
	} catch (err) {
		const msg = err instanceof DatabendUnavailableError ? err.message : String((err as Error)?.message ?? err);
		return { outcome: 'failed', cutoff, error: msg.slice(0, 300) };
	}
}
