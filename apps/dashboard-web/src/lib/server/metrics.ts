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
 * The rest of SR-3's chain now exists alongside this file: the scrape annotation on
 * `deploy/helm/sentinel/templates/dashboard.yaml` (`prometheus.io/scrape` + `/api/metrics`,
 * matching `processor.yaml`'s) and the alert rules in
 * `deploy/helm/sentinel/templates/prometheusrule.yaml`. §12 put Prometheus' own configuration in
 * the `infra` repo, which is not an implementing repo for this feature; the rules therefore ship
 * as a chart-rendered `PrometheusRule`, off by default, so the scheduling half and the alerting
 * half live together and neither can be forgotten.
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

/**
 * §12's `sentinel_report_escalations_total{outcome}`, alerted on as "`outcome="failed"` above
 * zero -- a report was assigned and nobody was told".
 *
 * Closed and fixed, exactly like `REPORT_OUTCOME`. There is deliberately no `unresolved` member:
 * §12 gives that state its own counter (`sentinel_oncall_schedule_unresolved_total`), and a state
 * counted twice under two names is a state whose two numbers eventually disagree.
 */
export const ESCALATION_OUTCOME = {
	/** §4.4 at intake: the on-call engineer was resolved, assigned and told. */
	Assigned: 'assigned',
	/** §4.4b: an unacknowledged deadline moved the report to the next untried member. */
	Reassigned: 'reassigned',
	/** The only value §12's alert fires on: escalation threw, so nobody was told. */
	Failed: 'failed',
} as const;

export type EscalationOutcome = (typeof ESCALATION_OUTCOME)[keyof typeof ESCALATION_OUTCOME];

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
	escalations: Counter<typeof LABEL_OUTCOME> | null;
	escalationsExhausted: Counter<string> | null;
	escalationsUnscoped: Counter<string> | null;
	oncallUnresolved: Counter<string> | null;
	releaseNextRewriteFailures: Counter<string> | null;
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

	// sentinel-releases-snooze §4.3 "Hourly" / §12 (decisions 15, 30): the dashboard's half of
	// `sentinel_release_next_rewrite_failures_total`. The processor registers a counter of the SAME
	// NAME in `packages/shared-go/obs/obs.go` for the post-commit fan-out's failures; this one counts
	// the hourly catch-up statement in `retention.ts` failing. Deliberately NO label -- not even a
	// `source` -- because the two processes' series are meant to sum by name at the scrape, and a
	// label on one side would make them two metrics. Initialised to zero for the same reason the
	// escalation counters are: a `> 0` alert over an absent series does not evaluate. Registered in
	// its own try so a failure here cannot take `sentinel_reports_received_total` down with it.
	let releaseNextRewriteFailures: Counter<string> | null = null;
	try {
		releaseNextRewriteFailures = new Counter({
			name: 'sentinel_release_next_rewrite_failures_total',
			help: "Failures of the hourly catch-up that rewrites resolved_in_version = 'next' to the earliest newer release (sentinel-releases-snooze §4.3); the processor's post-commit fan-out counts its failures under the same name.",
			registers: [registry],
		});
		releaseNextRewriteFailures.inc(0);
	} catch {
		releaseNextRewriteFailures = null;
	}

	// §12's three escalation signals. Two of them are the ONLY thing that notices a designed
	// silent state -- `exhausted` is §4.4's terminal "nobody answered", and `unresolved` is
	// "escalation ran for an org whose schedule resolves to nobody" -- which is why both are
	// initialised to zero below: an alert on `> 0` over a series that does not exist yet does not
	// evaluate, and "the counter is absent" would read exactly like "it never happened".
	let escalations: Counter<typeof LABEL_OUTCOME> | null = null;
	let escalationsExhausted: Counter<string> | null = null;
	let escalationsUnscoped: Counter<string> | null = null;
	let oncallUnresolved: Counter<string> | null = null;
	try {
		escalations = new Counter({
			name: 'sentinel_report_escalations_total',
			help: 'Report escalations by outcome (contract §4.4/§4.4b/§12).',
			labelNames: [LABEL_OUTCOME],
			registers: [registry],
		});
		for (const outcome of Object.values(ESCALATION_OUTCOME)) {
			escalations.labels({ [LABEL_OUTCOME]: outcome }).inc(0);
		}
		escalationsExhausted = new Counter({
			name: 'sentinel_report_escalations_exhausted_total',
			help: 'Escalation chains that walked every hop with no acknowledgement (contract §4.4).',
			registers: [registry],
		});
		escalationsExhausted.inc(0);
		// sentinel-project-environments §2.2, decisions 38/41: the unscoped fallback's OWN signal,
		// deliberately NOT `escalationsExhausted`. That one is §12's "nobody answered" alert, and a
		// chain that stopped because the resolved candidate does not hold the issue's environment
		// stopped on a GRANT rather than on a human -- firing it here would aim that alert at an
		// operator who was never asked anything. Same distinction `oncallUnresolved` already draws
		// one branch up. Unlabelled, because `organization_id`, `project_id` and `environment` are
		// exactly the values this file forbids as label dimensions.
		escalationsUnscoped = new Counter({
			name: 'sentinel_report_escalations_unscoped_total',
			help: 'Escalations whose resolved candidate did not hold the issue environment, so the whole rota was paged (contract §2.2, decisions 38/41).',
			registers: [registry],
		});
		escalationsUnscoped.inc(0);
		oncallUnresolved = new Counter({
			name: 'sentinel_oncall_schedule_unresolved_total',
			help: 'Escalations that ran for an organization whose on-call schedule resolved to nobody.',
			registers: [registry],
		});
		oncallUnresolved.inc(0);
	} catch {
		escalations = null;
		escalationsExhausted = null;
		escalationsUnscoped = null;
		oncallUnresolved = null;
	}

	return {
		registry,
		reportsReceived,
		escalations,
		escalationsExhausted,
		escalationsUnscoped,
		oncallUnresolved,
		releaseNextRewriteFailures,
	};
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

const KNOWN_ESCALATION_OUTCOMES = new Set<string>(Object.values(ESCALATION_OUTCOME));

/**
 * Increments `sentinel_report_escalations_total{outcome}`. Same closed-set runtime guard, and the
 * same never-throw contract, as `recordReportReceived`: escalation is called from a request that
 * has already created a report, and a metric must never be the thing that fails it.
 */
export function recordEscalation(outcome: EscalationOutcome): void {
	if (!KNOWN_ESCALATION_OUTCOMES.has(outcome)) return;
	state().escalations?.labels({ [LABEL_OUTCOME]: outcome }).inc();
}

/**
 * §12: "above zero -- a report walked the whole chain and no engineer acknowledged it. This is
 * the 'nobody answered' signal, and it is the only thing that notices §4.4's terminal state."
 * No labels: there is nothing to break the count down by that is not tenant-identifying.
 */
export function recordEscalationExhausted(): void {
	state().escalationsExhausted?.inc();
}

/**
 * sentinel-project-environments §2.2 (decisions 38, 41): above zero -- an escalation resolved a
 * candidate who does not hold the issue's environment, so the whole rota was paged with the
 * `unscoped` label instead. Its own counter and never `recordEscalationExhausted`, for the reason
 * stated where it is registered: the chain stopped on a grant, not on a human.
 */
export function recordEscalationUnscoped(): void {
	state().escalationsUnscoped?.inc();
}

/**
 * §12: "above zero -- escalation ran for an org whose schedule resolves to nobody." Unlabelled
 * for the same reason: the organization id is exactly the value that must never become a series.
 */
export function recordOncallScheduleUnresolved(): void {
	state().oncallUnresolved?.inc();
}

/**
 * sentinel-releases-snooze §4.3 "Hourly" (decision 30): above zero -- the retention sweep's
 * catch-up statement threw, so `next` issues wait one more hour for their version. Unlabelled by
 * contract (the processor's same-named counter and this one sum at the scrape); the SQLSTATE that
 * would have been the label goes on the `retention_cron.next_catchup_failed` log line instead.
 * Never throws: the sweep's other results must not depend on a counter.
 */
export function recordReleaseNextRewriteFailure(): void {
	state().releaseNextRewriteFailures?.inc();
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
