import { Registry, Counter } from '@prometheus-io/client';

/**
 * sentinel-user-report-issue contract SR-3 / §12 — dashboard-web's Prometheus metrics surface.
 *
 * **This file is the first one.** Verified at `b768e5b`: `apps/dashboard-web` had no `/metrics`
 * route and no Prometheus client at all (OTel *trace* exporters only, wired in
 * `src/lib/server/observability/`), so there was nothing to add a counter to. SR-3 says outright
 * that be.md's "follow `apps/processor-go/procmetrics`" does not port -- that is Go OTel bolted to
 * a Go health server. What DOES port is the two rules that file's doc comment states, and both are
 * enforced here:
 *
 * - **Fixed label constants, never a hand-typed string and never a caller-controlled one.** Every
 *   label value this process can emit is one of the `REPORT_OUTCOME` members below, and the
 *   recording helper takes that union rather than a `string`. A project name, an API key id, a
 *   reporter's `external_id` or a `context.brand` must never reach a label: they are
 *   attacker-controlled (§2) and unbounded, and one report flood would turn into one time series
 *   per reporter.
 * - **A metric that fails to register degrades to silence, never to a throw on the hot path.** An
 *   intake request must not 500 because a counter could not be created.
 *
 * **Aggregation under `replicaCount: 2` (SR-3 asks for this to be stated).** Every counter here is
 * process-local and monotonic within one process lifetime: a replica restart resets it to zero and
 * a scrape sees only the replica it hit. That is the ordinary Prometheus contract, and the
 * aggregation rule for every rule and dashboard SR-3's other half will write is therefore
 * `sum by (outcome) (rate(sentinel_reports_received_total[5m]))` -- summed ACROSS the pod targets,
 * with `rate()`/`increase()` absorbing the restart. There is no cross-replica shared counter and
 * deliberately so: it would need the dashboard-to-Redis dependency SR-1 already declined for the
 * throttle, to make a number Prometheus can compute itself.
 *
 * The scrape annotation on `deploy/helm/sentinel/templates/dashboard.yaml` and the alert rule
 * files are the rest of SR-3 and belong to the item that wires the metrics chain -- this one
 * stands the surface up and emits the first metric on it.
 */

/**
 * §12's first metric: `sentinel_reports_received_total{outcome}`, alerted on as
 * "`outcome="error"` ratio over 5 min above a threshold -- intake is broken and reports are being
 * lost".
 *
 * The value set is fixed and closed. `error` is `500` and nothing else, so the alert above means
 * what it says: a `400` from a malformed body is `rejected`, and a client sending garbage must not
 * page anybody.
 */
export const REPORT_OUTCOME = {
	/** `201`, a new report row. */
	Created: 'created',
	/** `201` replayed from `idempotency_key` within the 7-day window; nothing was created. */
	Deduplicated: 'deduplicated',
	/** `400` / `413` -- validation, a bound, or an unknown attachment id. */
	Rejected: 'rejected',
	/** `401` -- missing or unknown `X-API-Key`. */
	Unauthorized: 'unauthorized',
	/** `403` -- `forbidden_scope` or `project_not_found`. Deliberately one value for both: the two
	 *  responses are meant to be indistinguishable to the caller, and splitting the metric would
	 *  put the tenancy oracle back into Prometheus. */
	Forbidden: 'forbidden',
	/** `409` -- the same `idempotency_key` with a different body. */
	Conflict: 'conflict',
	/** `429` -- over the key's `rate_limit_rpm`. */
	RateLimited: 'ratelimited',
	/** `500` -- the only value §12's alert fires on. */
	Error: 'error',
} as const;

export type ReportOutcome = (typeof REPORT_OUTCOME)[keyof typeof REPORT_OUTCOME];

/** The one label this metric carries. Named once, so a typo cannot invent a second dimension. */
const LABEL_OUTCOME = 'outcome';

/**
 * The registry and its instruments are stashed on `globalThis` rather than held in module scope
 * alone. Vite's dev-server HMR re-evaluates a server module on edit; a second `new Counter({...})`
 * against a registry that already holds that name throws
 * `A metric with the name … has already been registered`, which would turn an unrelated edit into
 * a 500 on every intake request. Under `vite build` this is a plain module-level singleton.
 */
interface MetricsState {
	registry: Registry;
	reportsReceived: Counter<typeof LABEL_OUTCOME> | null;
}

const GLOBAL_KEY = Symbol.for('sentinel.dashboard.metrics');
const globalScope = globalThis as unknown as Record<symbol, MetricsState | undefined>;

function createState(): MetricsState {
	const registry = new Registry();
	// `registry.setDefaultLabels` and `collectDefaultMetrics` are deliberately NOT used: default
	// process metrics are not what §12 asks for, and a default label would land on every future
	// metric in this file without anyone choosing it.
	let reportsReceived: Counter<typeof LABEL_OUTCOME> | null = null;
	try {
		reportsReceived = new Counter({
			name: 'sentinel_reports_received_total',
			help: 'User reports received by POST /api/report, by outcome (contract §4.1/§12).',
			labelNames: [LABEL_OUTCOME],
			registers: [registry],
		});
		// Every series is initialised to zero. Without this, `outcome="error"` has no series at all
		// until the first failure, and a ratio alert over an absent series does not evaluate --
		// "intake is broken" would look exactly like "intake is quiet" for the first five minutes.
		for (const outcome of Object.values(REPORT_OUTCOME)) {
			reportsReceived.labels({ [LABEL_OUTCOME]: outcome }).inc(0);
		}
	} catch {
		// Registration only fails on a malformed name or a duplicate registration, neither of which
		// can happen with the fixed literal above -- but degrading to "this metric stops recording"
		// is still strictly better than failing an intake request. Mirrors procmetrics' nil guard.
		reportsReceived = null;
	}
	return { registry, reportsReceived };
}

function state(): MetricsState {
	const existing = globalScope[GLOBAL_KEY];
	if (existing) return existing;
	const created = createState();
	globalScope[GLOBAL_KEY] = created;
	return created;
}

/** The registry `/api/metrics` renders. Exported for tests; nothing else should reach into it. */
export function metricsRegistry(): Registry {
	return state().registry;
}

const KNOWN_OUTCOMES = new Set<string>(Object.values(REPORT_OUTCOME));

/**
 * Increments `sentinel_reports_received_total{outcome}`. Never throws, never blocks a response.
 *
 * The membership check is the unbounded-cardinality rule enforced at RUNTIME, not only in the type
 * system: a value outside the closed set is dropped rather than becoming a new time series. The
 * type already rejects it at every call site in this repo, but a metric label is exactly the place
 * where an untyped call (a `as never`, a JS caller, a future refactor) would turn one flood of
 * reports into one series per reporter, and Prometheus does not forgive that after the fact.
 */
export function recordReportReceived(outcome: ReportOutcome): void {
	if (!KNOWN_OUTCOMES.has(outcome)) return;
	state().reportsReceived?.labels({ [LABEL_OUTCOME]: outcome }).inc();
}

/** Renders the Prometheus text exposition format, with the content type a scraper expects. */
export async function renderMetrics(): Promise<{ body: string; contentType: string }> {
	const registry = metricsRegistry();
	return { body: await registry.metrics(), contentType: registry.contentType };
}

/**
 * Drops every recorded value. **Test-only** -- there is no operational reason to reset a counter,
 * and Prometheus reads a reset as a restart.
 */
export function resetMetricsForTest(): void {
	globalScope[GLOBAL_KEY] = undefined;
}
