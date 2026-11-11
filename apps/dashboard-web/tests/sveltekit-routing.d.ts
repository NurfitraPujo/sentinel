// tests/uuid-params.test.ts drives SvelteKit's own route parser and matcher runner, which the package does not export
// (or type). The subset it uses:
declare module '*/@sveltejs/kit/src/utils/routing.js' {
	interface RouteParam {
		name: string;
		matcher: string;
		optional: boolean;
		rest: boolean;
		chained: boolean;
	}
	export function parse_route_id(id: string): { pattern: RegExp; params: RouteParam[] };
	export function exec(
		match: RegExpMatchArray,
		params: RouteParam[],
		matchers: Record<string, (param: string) => boolean>
	): Record<string, string> | undefined;
	export function find_route<R extends { pattern: RegExp; params: RouteParam[] }>(
		path: string,
		routes: R[],
		matchers: Record<string, (param: string) => boolean>
	): { route: R; params: Record<string, string> } | null;
}
