import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { agentApiRegistry } from './registry';

/**
 * N6: walks `src/routes/api/agent/**\/+server.ts` with `fs`, extracts exported HTTP methods, maps
 * SvelteKit `[param]` directories to `{param}` OpenAPI-style paths, and asserts EXACT bidirectional
 * equality against the registry's (path, method) set -- so adding, removing, or renaming a route
 * (or forgetting to add/remove its registry entry) fails this test.
 */

const ROUTES_ROOT = path.resolve(
	path.dirname(fileURLToPath(import.meta.url)),
	'../../../routes/api/agent'
);

interface DiscoveredRoute {
	path: string;
	methods: string[];
	file: string;
}

function toOpenApiPath(relativeDir: string): string {
	const segments = relativeDir.split(path.sep).filter(Boolean);
	const mapped = segments.map((seg) => (seg.startsWith('[') && seg.endsWith(']') ? `{${seg.slice(1, -1)}}` : seg));
	return `/api/agent${mapped.length > 0 ? '/' + mapped.join('/') : ''}`;
}

const HTTP_METHOD_EXPORT = /export\s+const\s+(GET|POST|PATCH|PUT|DELETE)\s*[:=]/g;

function walk(dir: string, relative = ''): DiscoveredRoute[] {
	const entries = readdirSync(dir);
	const found: DiscoveredRoute[] = [];

	for (const entry of entries) {
		const fullPath = path.join(dir, entry);
		const stat = statSync(fullPath);
		if (stat.isDirectory()) {
			found.push(...walk(fullPath, path.join(relative, entry)));
			continue;
		}
		if (entry !== '+server.ts') continue;

		const source = readFileSync(fullPath, 'utf8');
		const methods = new Set<string>();
		for (const match of source.matchAll(HTTP_METHOD_EXPORT)) {
			methods.add(match[1]);
		}
		found.push({
			path: toOpenApiPath(relative),
			methods: [...methods],
			file: path.relative(path.resolve(ROUTES_ROOT, '../../../../..'), fullPath),
		});
	}

	return found;
}

function discoverRoutePathMethodPairs(): Set<string> {
	const routes = walk(ROUTES_ROOT);
	const pairs = new Set<string>();
	for (const route of routes) {
		for (const method of route.methods) {
			pairs.add(`${method.toLowerCase()} ${route.path}`);
		}
	}
	return pairs;
}

describe('agent API registry completeness', () => {
	it('covers exactly the routes under src/routes/api/agent/**/+server.ts', () => {
		const discovered = discoverRoutePathMethodPairs();
		const registered = new Set(agentApiRegistry.map((entry) => `${entry.method} ${entry.path}`));

		const missingFromRegistry = [...discovered].filter((pair) => !registered.has(pair)).sort();
		const staleInRegistry = [...registered].filter((pair) => !discovered.has(pair)).sort();

		expect(
			missingFromRegistry,
			`Route(s) exist under src/routes/api/agent but have no registry.ts entry: ${missingFromRegistry.join(', ')}`
		).toEqual([]);
		expect(
			staleInRegistry,
			`registry.ts has entries for route(s) that no longer exist: ${staleInRegistry.join(', ')}`
		).toEqual([]);
	});

	it('discovers at least one route (guards against a walk that silently finds nothing)', () => {
		expect(discoverRoutePathMethodPairs().size).toBeGreaterThan(0);
	});
});
