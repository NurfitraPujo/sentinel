/**
 * sentinel-issue-activity TASK-01 — a malformed id in a URL path never reaches Postgres.
 *
 * Production logged `PostgresError: invalid input syntax for type uuid: "--help"` (a 500) for
 * `GET /api/agent/issues/--help`, and the same for a truncated `42a26f64-`. The class is closed by routing: every route
 * parameter that is a `uuid` column carries the `uuid` matcher (`[issueId=uuid]`, src/params/uuid.ts), so a value that
 * is not a UUID matches NO route -- no handler, no load, no query runs -- and the request is the router's 404 (a JSON
 * `{ message }` for /api/**, hooks.server.ts's unmatchedApiHandle; the ordinary not-found page otherwise).
 *
 * This file walks src/routes and enforces it:
 *   1. every dynamic segment either has the matcher or is a NON-uuid parameter listed in NOT_UUID with the reason;
 *      a new `[fooId]` route fails here until it takes one side;
 *   2. for every route with a uuid parameter, the path with a malformed value (each of MALFORMED) in that parameter
 *      matches no route of the whole tree, using SvelteKit's own route parser and matcher runner, while the path with a
 *      real UUID matches it.
 */
import { describe, it, expect } from 'vitest';
import { readdirSync, statSync } from 'node:fs';
import path from 'node:path';
import { parse_route_id, find_route, exec } from '../node_modules/@sveltejs/kit/src/utils/routing.js';
import { match as uuidMatcher } from '../src/params/uuid';

const ROUTES_DIR = path.join(process.cwd(), 'src', 'routes');

/** Dynamic parameters that are deliberately NOT uuid-matched, each with why it cannot reach a uuid column unchecked. */
const NOT_UUID: Record<string, string> = {
	orgSlug: 'an organization slug (varchar)',
	traceId: 'a 32-hex trace id, validated by parseTraceId in the route (404 otherwise)',
	memberId: 'a membership id OR a user id (text); findOrgMember compares it with the uuid column only when isUuid',
	'agent/members:id': 'a membership id OR a user id (text); findOrgMember compares it with the uuid column only when isUuid',
	userId: 'a user id (text column)',
	token: 'an invitation token (hashed, compared as text)',
	os: 'a CLI build OS name',
	arch: 'a CLI build architecture name',
};

const MALFORMED = ['--help', '42a26f64-', '-h', 'not-a-uuid', '42a26f64-1b2c-4d5e-8f90-12345678901', '42a26f64-1b2c-4d5e-8f90-1234567890123', "1' OR '1'='1"];
const VALID = '42a26f64-1b2c-4d5e-8f90-123456789012';

interface RouteDir {
	id: string; // SvelteKit route id, e.g. /api/agent/issues/[issueId=uuid]/activity
}

function walk(dir: string, rel: string, out: RouteDir[]) {
	const entries = readdirSync(dir);
	if (entries.some((e) => /^\+(page|server)(\.server)?\.(ts|js|svelte)$/.test(e))) {
		out.push({ id: '/' + rel.split(path.sep).filter(Boolean).join('/') });
	}
	for (const e of entries) {
		const full = path.join(dir, e);
		if (statSync(full).isDirectory()) walk(full, path.join(rel, e), out);
	}
}

const routes: RouteDir[] = [];
walk(ROUTES_DIR, '', routes);

const parsed = routes.map((r) => ({ id: r.id, ...parse_route_id(r.id) }));
const matchers = { uuid: uuidMatcher };

function segmentsOf(id: string) {
	return id.split('/').filter(Boolean);
}

describe('the uuid param matcher', () => {
	it('accepts a canonical UUID in either case and nothing else', () => {
		expect(uuidMatcher(VALID)).toBe(true);
		expect(uuidMatcher(VALID.toUpperCase())).toBe(true);
		for (const bad of [...MALFORMED, '', VALID.replace(/-/g, ''), `{${VALID}}`]) expect(uuidMatcher(bad)).toBe(false);
	});
});

describe('every dynamic route segment is uuid-matched or deliberately not (src/routes walk)', () => {
	it('finds the routes', () => {
		expect(routes.length).toBeGreaterThan(50);
		expect(routes.some((r) => r.id === '/api/agent/issues/[issueId=uuid]')).toBe(true);
		expect(routes.some((r) => r.id === '/issues/[id=uuid]')).toBe(true);
	});

	it('has no unmatched parameter outside NOT_UUID', () => {
		const undecided: string[] = [];
		for (const r of routes) {
			for (const seg of segmentsOf(r.id)) {
				const m = /^\[([A-Za-z]+)(?:=([a-z]+))?\]$/.exec(seg);
				if (!m) continue;
				if (m[2] === 'uuid') continue;
				const key = r.id.startsWith('/api/agent/members/') ? `agent/members:${m[1]}` : m[1];
				if (!(key in NOT_UUID)) undecided.push(`${r.id} (${seg})`);
			}
		}
		expect(
			undecided,
			'A route parameter is neither `=uuid`-matched nor listed in NOT_UUID. If it is a uuid column, rename the ' +
				'directory to [name=uuid]; otherwise add it to NOT_UUID with the reason a malformed value cannot reach a uuid column.'
		).toEqual([]);
	});
});

describe('a malformed id in any uuid parameter matches no route (so no handler and no query runs)', () => {
	const uuidRoutes = parsed.filter((r) => r.params.some((p: { matcher?: string }) => p.matcher === 'uuid'));

	it('covers every uuid-param route (the list the final report enumerates)', () => {
		expect(uuidRoutes.length).toBeGreaterThanOrEqual(58);
	});

	for (const route of uuidRoutes) {
		const uuidParams = route.params.filter((p: { matcher?: string }) => p.matcher === 'uuid').map((p: { name: string }) => p.name);
		for (const param of uuidParams) {
			it(`${route.id} — [${param}]`, () => {
				const build = (value: string) =>
					'/' +
					segmentsOf(route.id)
						.map((seg) => {
							const m = /^\[([A-Za-z]+)(?:=[a-z]+)?\]$/.exec(seg);
							if (!m) return seg;
							if (m[1] === param) return value;
							return m[1] === 'orgSlug' ? 'acme' : VALID;
						})
						.join('/');

				const good = build(VALID);
				const goodMatch = route.pattern.exec(good);
				expect(goodMatch && exec(goodMatch, route.params, matchers), `${good} should match ${route.id}`).toBeTruthy();

				for (const bad of MALFORMED) {
					const url = build(encodeURIComponent(bad));
					// SvelteKit matches the DECODED pathname.
					const found = find_route(decodeURIComponent(url), parsed, matchers);
					expect(found?.route.id ?? null, `${url} must match no route`).toBeNull();
				}
			});
		}
	}
});
