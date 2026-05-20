import { db } from '$lib/server/db';
import { errorOccurrences, issues } from '$lib/db/schema';
import { sql, lt, and, eq } from 'drizzle-orm';

export interface RetentionResult {
	deletedOccurrences: number;
	markedStaleIssues: number;
	deletedOrphanedIssues: number;
	retentionDays: number;
	cutoffDate: Date;
}

export async function cleanupRetainedData(retentionDays: number = 30): Promise<RetentionResult> {
	const cutoffDate = new Date();
	cutoffDate.setDate(cutoffDate.getDate() - retentionDays);

	const deletedOccurrencesResult = await db
		.delete(errorOccurrences)
		.where(lt(errorOccurrences.createdAt, cutoffDate))
		.returning({ id: errorOccurrences.id });

	const deletedOccurrences = deletedOccurrencesResult.length;

	const staleIssuesResult = await db
		.update(issues)
		.set({ status: 'stale' })
		.where(
			and(
				sql`${issues.id} NOT IN (
					SELECT DISTINCT ${errorOccurrences.issueId}
					FROM ${errorOccurrences}
				)`,
				eq(issues.status, 'open')
			)
		)
		.returning({ id: issues.id });

	const markedStaleIssues = staleIssuesResult.length;

	const orphanedIssuesResult = await db
		.delete(issues)
		.where(
			sql`${issues.id} NOT IN (
				SELECT DISTINCT ${errorOccurrences.issueId}
				FROM ${errorOccurrences}
			)`
		)
		.returning({ id: issues.id });

	const deletedOrphanedIssues = orphanedIssuesResult.length;

	return {
		deletedOccurrences,
		markedStaleIssues,
		deletedOrphanedIssues,
		retentionDays,
		cutoffDate,
	};
}
