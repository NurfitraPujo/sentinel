import { describe, it, expect, vi, beforeEach } from 'vitest';
import { PgDialect } from 'drizzle-orm/pg-core';
import type { SQL } from 'drizzle-orm';

/**
 * sentinel-user-report-issue §4.6 / OQ-6: the `external_reporters` upsert.
 *
 * The behaviour under test is the CONFLICT clause, because that is the whole reason the table
 * carries `UNIQUE (project_id, external_id)`: a repeat reporter must be one row seen twice, not a
 * second person. The COALESCE pair is equally load-bearing -- §3 says an omitted field and an
 * explicit null both mean "absent" on intake, so a report that omits `display_name` must not blank
 * out the name an earlier one supplied.
 */

const captured: { values?: any; conflict?: any } = {};

const dbMock: any = {
	insert: vi.fn(() => dbMock),
	values: vi.fn((v: unknown) => {
		captured.values = v;
		return dbMock;
	}),
	onConflictDoUpdate: vi.fn((c: unknown) => {
		captured.conflict = c;
		return dbMock;
	}),
	select: vi.fn(() => dbMock),
	from: vi.fn(() => dbMock),
	where: vi.fn(() => dbMock),
	returning: vi.fn(async () => [{ id: 'reporter-1' }]),
	then: vi.fn((resolve: any) => resolve([{ id: 'reporter-1' }])),
};

vi.mock('$lib/server/db', () => ({ db: dbMock }));

const { upsertExternalReporter } = await import('./external-reporters');

const dialect = new PgDialect();
function render(value: unknown): string {
	const { sql, params } = dialect.sqlToQuery(value as SQL);
	return `${sql} -- ${params.map(String).join(', ')}`;
}

beforeEach(() => {
	vi.clearAllMocks();
	captured.values = undefined;
	captured.conflict = undefined;
});

describe('upsertExternalReporter', () => {
	it('inserts the credential-derived tenant columns alongside the external id', async () => {
		await upsertExternalReporter({
			organizationId: 'org-1',
			projectId: 'proj-1',
			externalId: 'u_8f3c41',
			displayName: 'Ada',
			email: 'ada@example.test',
		});
		expect(captured.values).toEqual({
			organizationId: 'org-1',
			projectId: 'proj-1',
			externalId: 'u_8f3c41',
			displayName: 'Ada',
			email: 'ada@example.test',
		});
	});

	it('conflicts on (project_id, external_id) — the identity rule, not on the id', async () => {
		await upsertExternalReporter({ organizationId: 'org-1', projectId: 'proj-1', externalId: 'u_1' });
		const target = captured.conflict.target.map((c: any) => c.name);
		expect(target).toEqual(['project_id', 'external_id']);
	});

	it('advances last_seen_at on conflict and never touches first_seen_at', async () => {
		await upsertExternalReporter({ organizationId: 'org-1', projectId: 'proj-1', externalId: 'u_1' });
		expect(Object.keys(captured.conflict.set).sort()).toEqual(['displayName', 'email', 'lastSeenAt']);
		expect(render(captured.conflict.set.lastSeenAt)).toContain('now()');
	});

	it('coalesces display_name/email so an omitted field does not blank a stored one', async () => {
		await upsertExternalReporter({ organizationId: 'org-1', projectId: 'proj-1', externalId: 'u_1' });
		expect(render(captured.conflict.set.displayName)).toContain('coalesce(excluded.display_name');
		expect(render(captured.conflict.set.email)).toContain('coalesce(excluded.email');
	});

	it('normalises whitespace-only optional fields to null rather than storing "  "', async () => {
		await upsertExternalReporter({
			organizationId: 'org-1',
			projectId: 'proj-1',
			externalId: 'u_1',
			displayName: '   ',
			email: undefined,
		});
		expect(captured.values.displayName).toBeNull();
		expect(captured.values.email).toBeNull();
	});

	it('trims a supplied display name', async () => {
		await upsertExternalReporter({
			organizationId: 'org-1',
			projectId: 'proj-1',
			externalId: 'u_1',
			displayName: '  Ada  ',
		});
		expect(captured.values.displayName).toBe('Ada');
	});

	it('throws rather than returning undefined when the insert returns no row', async () => {
		dbMock.returning = vi.fn(async () => []);
		await expect(
			upsertExternalReporter({ organizationId: 'org-1', projectId: 'proj-1', externalId: 'u_1' })
		).rejects.toThrow(/Failed to upsert external reporter/);
		dbMock.returning = vi.fn(async () => [{ id: 'reporter-1' }]);
	});
});
