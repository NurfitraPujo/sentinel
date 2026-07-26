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
					prefix: 'sk_test_',
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
		expect(getByText('No API keys found.')).toBeTruthy();
	});
});
