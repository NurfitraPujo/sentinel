import { enumLiteral, uuidLiteral } from './literals';

/**
 * sentinel-reporting-and-dashboard D11: access is enforced IN THE SQL WHERE CLAUSE, never by filtering results.
 *
 * A Scope is one project the viewer can reach and the environments they may see in it (`null` = every environment,
 * as `resolveEnvironmentScopes` reports an unrestricted project; an array = exactly those, possibly empty).
 * The predicate is built only from Scopes, so a project missing from the list, or an environment outside a
 * restricted scope, cannot contribute a row (and so cannot contribute even a count). No scopes at all is the
 * constant-false predicate: the empty state.
 *
 * `declaredEnvironments` is the allow-list for D14's validation of environment names (names are free text in
 * Postgres); a name outside it is rejected before it reaches SQL.
 */
export interface Scope {
	projectId: string;
	environments: string[] | null;
}

export interface AccessFilter {
	scopes: Scope[];
	/** 'all' or one environment name; only meaningful with a single project (the dashboard coerces it). */
	environment: string;
	/** Every environment name that exists for the viewer's projects: the allow-list for `environment` and scope names. */
	knownEnvironments: readonly string[];
}

/**
 * The predicate over `projectCol` (the issue's project_id) and `envCol` (the row's environment). Pass the aliased
 * column expressions, e.g. `i.project_id` and `o.environment`.
 */
export function accessPredicate(f: AccessFilter, projectCol: string, envCol: string): string {
	const clauses: string[] = [];
	for (const s of f.scopes) {
		const project = `${projectCol} = ${uuidLiteral(s.projectId)}`;
		let env = '';
		if (s.environments !== null) {
			if (s.environments.length === 0) continue; // a restricted scope with no environments sees nothing
			env = ` AND ${envCol} IN (${s.environments.map((e) => enumLiteral(e, f.knownEnvironments)).join(', ')})`;
		}
		if (f.environment !== 'all') {
			env += ` AND ${envCol} = ${enumLiteral(f.environment, f.knownEnvironments)}`;
		}
		clauses.push(`(${project}${env})`);
	}
	if (clauses.length === 0) return '(1 = 0)';
	return `(${clauses.join(' OR ')})`;
}

/** Project-only predicate, for tables with no environment column (releases windows are built from occurrences). */
export function projectPredicate(f: AccessFilter, projectCol: string): string {
	const ids = f.scopes.filter((s) => s.environments === null || s.environments.length > 0).map((s) => uuidLiteral(s.projectId));
	return ids.length === 0 ? '(1 = 0)' : `${projectCol} IN (${ids.join(', ')})`;
}
