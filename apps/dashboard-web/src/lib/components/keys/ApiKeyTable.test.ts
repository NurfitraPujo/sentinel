import { describe, it, expect } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ApiKeyTable from './ApiKeyTable.svelte';

describe('ApiKeyTable Component', () => {
	it('renders table with keys', () => {
		const { getByText } = render(ApiKeyTable, {
			keys: [
				{
					id: '1',
					name: 'Test Key',
					keyPrefix: 'sk_test_',
					scopes: ['Read/Query'],
					targetProject: 'All Projects',
					status: 'active',
					createdAt: '2023-01-01'
				}
			]
		});

		expect(getByText('Test Key')).toBeTruthy();
		expect(getByText('sk_test_••••')).toBeTruthy();
		expect(getByText('Read/Query')).toBeTruthy();
	});

	it('shows empty state when no keys', () => {
		const { getByText } = render(ApiKeyTable, { keys: [] });
		expect(getByText(/No API keys found/i)).toBeTruthy();
	});

	// D25 regression guard: the API (getOrganizationApiKeys / toPublicKey, see
	// src/lib/db/queries/apikeys.ts:26,47 and src/routes/api/organizations/keys.test.ts) returns
	// `keyPrefix`, never `prefix`. A row shaped exactly like what GET /keys actually returns must
	// render the real prefix, not silently fall back to the literal 'sent_' placeholder — that
	// fallback is precisely what let a field-name mismatch ship past a green suite before, because
	// the old test fed a hand-written `prefix: 'sk_test_'` object that no real API response has.
	it('renders the real keyPrefix from an API-shaped row, not the sent_ fallback', () => {
		const apiShapedKey = {
			id: 'key-1',
			organizationId: 'org-1',
			projectId: null,
			name: 'Production Ingestion',
			keyPrefix: 'sent_org_',
			scope: 'ingest',
			status: 'active',
			rateLimitRpm: 5000,
			expiresAt: null,
			revokedAt: null,
			createdBy: 'user-1',
			createdAt: '2026-08-01T00:00:00.000Z',
		};

		const { getByText, queryByText } = render(ApiKeyTable, { keys: [apiShapedKey] });

		expect(getByText('sent_org_••••')).toBeTruthy();
		// The fallback literal must not appear anywhere for a row that has a real keyPrefix.
		expect(queryByText('sent_••••')).toBeNull();
	});
});
