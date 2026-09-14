import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/svelte';
import type { Component } from 'svelte';
import Page from './+page.svelte';

// sentinel-releases-snooze fe.md task 1, contract §11's `@testing-library/svelte` row — flow `R1`
// (decisions 25, 28): a third link, *Releases*, in each row's `.project-links` after *Settings*,
// rendered for EVERY row and every member — there is no `releasesVisible` on page data and none is
// coming; an environment-scoped member sees the link and the loader answers `404 Project not
// found` (decision 20). Page data is a literal typed by hand; no DB, no loader.

interface ProjectsPageData {
	org: { name: string; slug: string };
	projects: Array<{ id: string; name: string; createdAt: Date }>;
	canCreate: boolean;
}

const ProjectsPage = Page as unknown as Component<{ data: ProjectsPageData; form: null }>;

function pageData(overrides: Partial<ProjectsPageData> = {}): ProjectsPageData {
	return {
		org: { name: 'Acme', slug: 'acme' },
		projects: [
			{ id: 'proj-a', name: 'Checkout API', createdAt: new Date('2026-09-01T00:00:00Z') },
			{ id: 'proj-b', name: 'Web app', createdAt: new Date('2026-09-02T00:00:00Z') },
			{ id: 'proj-c', name: 'Batch jobs', createdAt: new Date('2026-09-03T00:00:00Z') },
		],
		canCreate: false,
		...overrides,
	};
}

function linksOfEachRow(): Array<Array<[string, string | null]>> {
	return Array.from(document.querySelectorAll('.project-row')).map((row) =>
		Array.from(row.querySelectorAll('.project-links a')).map(
			(a) => [(a.textContent ?? '').trim(), a.getAttribute('href')] as [string, string | null]
		)
	);
}

describe('/{orgSlug}/projects the Releases link (contract §4.4, flow R1)', () => {
	it('renders API keys · Settings · Releases on every row, the third pointing at the releases page', () => {
		render(ProjectsPage, { data: pageData(), form: null });
		expect(linksOfEachRow()).toEqual([
			[
				['API keys', '/acme/projects/proj-a/settings/keys'],
				['Settings', '/acme/projects/proj-a/settings'],
				['Releases', '/acme/projects/proj-a/releases'],
			],
			[
				['API keys', '/acme/projects/proj-b/settings/keys'],
				['Settings', '/acme/projects/proj-b/settings'],
				['Releases', '/acme/projects/proj-b/releases'],
			],
			[
				['API keys', '/acme/projects/proj-c/settings/keys'],
				['Settings', '/acme/projects/proj-c/settings'],
				['Releases', '/acme/projects/proj-c/releases'],
			],
		]);
	});

	it('renders the link unconditionally — for a member who cannot create projects too (decision 28)', () => {
		render(ProjectsPage, { data: pageData({ canCreate: false }), form: null });
		const releasesLinks = Array.from(document.querySelectorAll('.project-links a')).filter(
			(a) => (a.textContent ?? '').trim() === 'Releases'
		);
		expect(releasesLinks.length).toBe(3);
	});

	it('renders the link for a write role as well', () => {
		render(ProjectsPage, { data: pageData({ canCreate: true }), form: null });
		const releasesLinks = Array.from(document.querySelectorAll('.project-links a')).filter(
			(a) => (a.textContent ?? '').trim() === 'Releases'
		);
		expect(releasesLinks.length).toBe(3);
	});
});
