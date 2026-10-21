import { sql } from 'drizzle-orm';
import { db } from '$lib/server/db';
import type { SliHourRow, SliStore } from './collect';

/** The Postgres-backed SliStore: `sli_hourly` (migration 1726100000). */
export const pgSliStore: SliStore = {
	async hasRows(sliId, environment) {
		const res = await db.execute(
			sql`SELECT 1 AS present FROM sli_hourly WHERE sli_id = ${sliId} AND environment = ${environment} LIMIT 1`
		);
		return (res as unknown as unknown[]).length > 0;
	},
	async upsert(rows: SliHourRow[]) {
		await db.transaction(async (tx) => {
			// Chunked so a 30-day backfill of four SLIs (<= ~2,900 rows) stays under the 65k parameter limit.
			for (let i = 0; i < rows.length; i += 500) {
				const chunk = rows.slice(i, i + 500);
				const values = sql.join(
					chunk.map((r) => sql`(${r.sliId}, ${r.environment}, ${r.hourStart.toISOString()}::timestamptz, ${r.total}, ${r.errors}, ${r.fast}, ${r.latencyTotal})`),
					sql`, `
				);
				await tx.execute(sql`
					INSERT INTO sli_hourly (sli_id, environment, hour_start, total, errors, fast, latency_total)
					VALUES ${values}
					ON CONFLICT (sli_id, environment, hour_start)
					DO UPDATE SET total = EXCLUDED.total, errors = EXCLUDED.errors, fast = EXCLUDED.fast, latency_total = EXCLUDED.latency_total
				`);
			}
		});
	},
};
