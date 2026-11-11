import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/svelte';
import ApiKeyCreateModal from './ApiKeyCreateModal.svelte';

// sentinel-relay-key-trust §4.3: Relay is offered when creating a project-scoped key, never an org-wide one.
const scopeValues = (container: HTMLElement) =>
	Array.from(container.querySelectorAll<HTMLInputElement>('input[name="scope-group"]')).map((i) => i.value);

describe('ApiKeyCreateModal relay scope', () => {
	const projects = [{ id: 'p1', name: 'mobile' }];

	it('offers Relay, with the warning, on the project keys page (allowOrgWide=false)', () => {
		const { container, getByText } = render(ApiKeyCreateModal, { isOpen: true, projects, allowOrgWide: false });
		expect(scopeValues(container)).toContain('relay');
		expect(getByText(/for core's mobile relay only/i)).toBeTruthy();
		expect(getByText(/Keep it in core's secrets/i)).toBeTruthy();
	});

	it('does not offer Relay on the org-wide keys modal (allowOrgWide=true)', () => {
		const { container } = render(ApiKeyCreateModal, { isOpen: true, projects, allowOrgWide: true });
		expect(scopeValues(container)).toEqual(['ingest', 'read', 'admin']);
	});
});
