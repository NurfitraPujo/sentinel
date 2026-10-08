/**
 * sentinel-reporting-and-dashboard D7: the SLI configuration. It lives in Helm values (`serviceSlis`), is rendered to
 * env `SERVICE_SLI_CONFIG` as JSON and is validated here. A bad config throws with every problem named, so a typo fails
 * the pod at start rather than the first hourly run.
 *
 * Lives under `$lib/server/`, not in a route file (B12): a route may export only SvelteKit's allowlisted names.
 */

export type SliKind = 'http' | 'jobs' | 'nats';

export interface ServiceSli {
	id: string;
	kind: SliKind;
	/** The Sentinel project the SLI belongs to, by NAME (projects are resolved from Postgres, D2). */
	project: string;
	/** http: the Envoy cluster name (`api_core_80`). jobs/nats: the metric name, informational. */
	source: string;
	/** Fraction in (0,1), e.g. 0.995. Absent for tracked, untargeted rates. */
	availabilityTarget?: number;
	/** http only: the edge histogram bucket `le` (ms, spelled as the histogram spells it) and the fraction under it. */
	latency?: { le: string; target: number };
	/** D19 group-1/2/3 event matching for errors-per-1k: which stored events count against this SLI. */
	service?: string;
	windowDays: number;
}

/**
 * The edge histogram's bucket bounds in ms (Envoy's default `envoy_cluster_upstream_rq_time_bucket` set, spelled
 * exactly as the `le` label spells them). A latency `le` must be one of these (D7): any other value matches no series
 * and would silently write no `fast`.
 */
export const EDGE_LATENCY_BUCKETS = [
	'0.5', '1.0', '5.0', '10.0', '25.0', '50.0', '100.0', '250.0', '500.0', '1000.0', '2500.0', '5000.0',
	'10000.0', '30000.0', '60000.0', '300000.0', '600000.0', '1800000.0', '3600000.0',
] as const;

const KINDS: readonly SliKind[] = ['http', 'jobs', 'nats'];

export function parseServiceSliConfig(raw: string | undefined): ServiceSli[] {
	if (raw === undefined || raw.trim() === '') return [];
	let parsed: unknown;
	try {
		parsed = JSON.parse(raw);
	} catch (err) {
		throw new Error(`SERVICE_SLI_CONFIG is not valid JSON: ${(err as Error).message}`);
	}
	if (!Array.isArray(parsed)) throw new Error('SERVICE_SLI_CONFIG must be a JSON array');

	const problems: string[] = [];
	const seen = new Set<string>();
	const out: ServiceSli[] = [];
	parsed.forEach((entry, i) => {
		const at = `serviceSlis[${i}]`;
		if (typeof entry !== 'object' || entry === null) {
			problems.push(`${at} must be an object`);
			return;
		}
		const e = entry as Record<string, unknown>;
		const str = (k: string): string | undefined => (typeof e[k] === 'string' && (e[k] as string).trim() ? (e[k] as string) : undefined);
		const id = str('id');
		if (!id || !/^[a-z0-9][a-z0-9-]{0,63}$/.test(id)) problems.push(`${at}.id must match [a-z0-9-]{1,64}`);
		else if (seen.has(id)) problems.push(`${at}.id "${id}" is duplicated`);
		else seen.add(id);
		const kind = str('kind') as SliKind | undefined;
		if (!kind || !KINDS.includes(kind)) problems.push(`${at}.kind must be one of ${KINDS.join(', ')}`);
		const project = str('project');
		if (!project) problems.push(`${at}.project is required`);
		const source = str('source');
		if (!source) problems.push(`${at}.source is required`);
		if (kind === 'http' && source && !/^[A-Za-z0-9_.:-]+$/.test(source)) problems.push(`${at}.source is not a valid Envoy cluster name`);

		let availabilityTarget: number | undefined;
		if (e.availabilityTarget !== undefined) {
			const t = e.availabilityTarget;
			if (typeof t !== 'number' || !(t > 0 && t < 1)) problems.push(`${at}.availabilityTarget must be a number in (0,1)`);
			else availabilityTarget = t;
		}
		let latency: ServiceSli['latency'];
		if (e.latency !== undefined) {
			const l = e.latency as Record<string, unknown> | null;
			if (kind !== 'http') problems.push(`${at}.latency is only valid for kind http`);
			else if (!l || typeof l.le !== 'string' || !(EDGE_LATENCY_BUCKETS as readonly string[]).includes(l.le))
				problems.push(`${at}.latency.le must be one of the edge histogram buckets: ${EDGE_LATENCY_BUCKETS.join(', ')}`);
			else if (typeof l.target !== 'number' || !(l.target > 0 && l.target < 1)) problems.push(`${at}.latency.target must be a number in (0,1)`);
			else latency = { le: l.le, target: l.target };
		}
		const service = e.service === undefined ? undefined : str('service');
		if (e.service !== undefined && !service) problems.push(`${at}.service must be a non-empty string`);
		const windowDays = e.windowDays === undefined ? 28 : e.windowDays;
		if (typeof windowDays !== 'number' || !Number.isInteger(windowDays) || windowDays < 1 || windowDays > 90)
			problems.push(`${at}.windowDays must be an integer 1..90`);

		if (id && kind && KINDS.includes(kind) && project && source && typeof windowDays === 'number') {
			out.push({ id, kind, project, source, availabilityTarget, latency, service, windowDays });
		}
	});
	if (problems.length) throw new Error(`SERVICE_SLI_CONFIG is invalid:\n- ${problems.join('\n- ')}`);
	return out;
}
