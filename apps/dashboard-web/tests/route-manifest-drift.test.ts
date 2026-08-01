/**
 * Route-manifest drift guard (D01 / P2-1).
 *
 * hooks.server.ts's `orgHandle` decides whether a top-level URL segment is an org slug by checking it
 * against a hand-maintained `reservedRoutes` array. That list silently swallowed any new top-level
 * route added under src/routes/ — exactly what happened to `invitations`: the route existed, but
 * because it wasn't in the list, orgHandle parsed `/invitations/<token>` as an org slug and an
 * authenticated request got a 403 instead of ever reaching the route's redirect.
 *
 * This test enumerates the real top-level directories under src/routes/ and fails loudly if one shows
 * up that is neither in reservedRoutes nor the dynamic org-slug segment (`[orgSlug]`) — i.e. neither
 * "yes this is reserved" nor "yes this is deliberately the org-slug catch-all" has been decided for it.
 * That is the "add a test that enumerates top-level directories... and FAILS when one is neither
 * reserved nor intentionally an org slug" acceptance bar from the plan.
 */
import { describe, it, expect } from 'vitest';
import { readdirSync, statSync, readFileSync } from 'node:fs';
import path from 'node:path';

// process.cwd() is the package root (apps/dashboard-web) when vitest runs, whether invoked from the
// repo root via a workspace filter or from inside this package directly.
const PACKAGE_ROOT = process.cwd();
const ROUTES_DIR = path.join(PACKAGE_ROOT, 'src', 'routes');

// Mirrors hooks.server.ts's reservedRoutes exactly. Kept as a separate literal (not imported) on
// purpose: hooks.server.ts pulls in $lib/server/db, $lib/db/schema and $lib/server/auth-config at
// module load, which this test has no business mocking just to read an array. Divergence between this
// literal and the real one is caught below by parsing hooks.server.ts's source text directly, so the
// two can't quietly drift apart from each other either.
const EXPECTED_RESERVED_ROUTES = [
	'api',
	'auth',
	'issues',
	'search',
	'settings',
	'admin',
	'docs',
	'billing',
	'support',
	'signin',
	'invitations',
];

// The dynamic segment that IS the org-slug catch-all, by design.
const ORG_SLUG_SEGMENT = '[orgSlug]';

function topLevelRouteDirs(): string[] {
	return readdirSync(ROUTES_DIR).filter((entry) => statSync(path.join(ROUTES_DIR, entry)).isDirectory());
}

function reservedRoutesFromSource(): string[] {
	const hooksSrc = path.join(PACKAGE_ROOT, 'src', 'hooks.server.ts');
	const text = readFileSync(hooksSrc, 'utf-8');
	const match = text.match(/const reservedRoutes = \[([\s\S]*?)\];/);
	if (!match) throw new Error('route-manifest-drift.test.ts: could not locate reservedRoutes array in hooks.server.ts — did it get renamed or refactored to derive from the manifest? Update this test accordingly.');
	return Array.from(match[1].matchAll(/'([^']+)'/g)).map((m) => m[1]);
}

describe('route manifest vs hooks.server.ts reservedRoutes (D01)', () => {
	it('sanity: src/routes has more than a couple of top-level directories', () => {
		expect(topLevelRouteDirs().length).toBeGreaterThan(2);
	});

	it('this test file\'s expected list matches the literal reservedRoutes array in hooks.server.ts', () => {
		expect(new Set(reservedRoutesFromSource())).toEqual(new Set(EXPECTED_RESERVED_ROUTES));
	});

	it('every top-level route directory is either reserved or the deliberate org-slug catch-all', () => {
		const reserved = new Set(reservedRoutesFromSource());
		const undecided = topLevelRouteDirs().filter(
			(dir) => dir !== ORG_SLUG_SEGMENT && !reserved.has(dir)
		);

		expect(
			undecided,
			`New top-level route director${undecided.length === 1 ? 'y' : 'ies'} ${JSON.stringify(undecided)} ` +
				`under src/routes/ ${undecided.length === 1 ? 'is' : 'are'} neither in hooks.server.ts's ` +
				`reservedRoutes nor the '${ORG_SLUG_SEGMENT}' org-slug catch-all. orgHandle runs before route ` +
				`resolution, so an authenticated request to this path will be parsed as an org slug and 403 ` +
				`instead of reaching the route (this is exactly the D01/invitations bug). Add it to ` +
				`reservedRoutes in src/hooks.server.ts if it is NOT an org slug, or explicitly acknowledge it ` +
				`belongs to the org-slug space if it somehow is.`
		).toEqual([]);
	});
});
