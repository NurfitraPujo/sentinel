import { describe, it, expect } from 'vitest';
import { readFileSync, existsSync } from 'fs';
import path from 'path';

const ROUTES_DIR = path.join(process.cwd(), 'src/routes');
const LAYOUT = readFileSync(path.join(ROUTES_DIR, '+layout.svelte'), 'utf-8');

// Every href on a .nav-link in the header, with Svelte interpolation collapsed to the dynamic
// segment it stands for. `/{data.orgSlug}/reports` is a link to the `[orgSlug]/reports` route.
function navTargets(): string[] {
	const targets: string[] = [];
	for (const m of LAYOUT.matchAll(/<a\s+href=(?:"([^"]*)"|\{`([^`]*)`\}|\{[^}]*?`([^`]*)`[^}]*\})[^>]*class="nav-link/g)) {
		const raw = m[1] ?? m[2] ?? m[3] ?? '';
		if (raw) targets.push(raw);
	}
	return targets;
}

function routeExists(href: string): boolean {
	const segments = href
		.replace(/\$\{[^}]*\}/g, '[dyn]')
		.replace(/\{[^}]*\}/g, '[dyn]')
		.split('?')[0]
		.split('/')
		.filter(Boolean);

	let dir = ROUTES_DIR;
	for (const segment of segments) {
		if (segment === '[dyn]') {
			// A dynamic segment must land on the org-slug catch-all; nothing else in the nav is
			// parameterised.
			dir = path.join(dir, '[orgSlug]');
		} else {
			dir = path.join(dir, segment);
		}
		if (!existsSync(dir)) return false;
	}
	// A route is only navigable if it renders something — a directory with only a +layout or a
	// +server file is not a page. This is what /issues and /settings were before PR #6: real
	// directories, no index, 404 for the user.
	return existsSync(path.join(dir, '+page.svelte')) || existsSync(path.join(dir, '+page.server.ts'));
}

describe('header navigation', () => {
	it('finds the nav links to assert on', () => {
		expect(navTargets().length).toBeGreaterThanOrEqual(5);
	});

	it('every nav destination resolves to a route that renders a page', () => {
		const broken = navTargets().filter((href) => !routeExists(href));
		expect(broken, `nav links with no page behind them: ${JSON.stringify(broken)}`).toEqual([]);
	});

	// /search had exactly one inbound link in the whole app — its own "Clear" button — so the page
	// existed and worked but could not be found from anywhere.
	it('links /search, which was previously reachable only by typing the URL', () => {
		expect(navTargets()).toContain('/search');
	});
});
