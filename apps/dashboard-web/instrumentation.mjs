// @ts-nocheck -- this file is deliberately outside the SvelteKit/TS project's own graph (see below): it
// isn't in .svelte-kit/tsconfig.json's `include`, and `pnpm check` never saw it until
// tests/instrumentation-redaction.test.ts started importing `redactQueryString` from it, at which point
// TS pulled it in transitively to type the import and flagged every implicit-any in a plain Node bootstrap
// script that predates this change. Annotating a hand-rolled degradation script with full JSDoc types for
// OTel's Span/IncomingMessage shapes would add noise without catching real bugs here — the one function
// worth type safety (`redactQueryString`) is covered by that same test file instead.
//
// OpenTelemetry Node SDK bootstrap for the dashboard's SvelteKit server (best-effort C in
// docs/plans/OBSERVABILITY_PLAN.md D-f). Deliberately NOT under src/ and NOT imported from any
// SvelteKit module: it must never pass through Vite/rollup's bundling. OTel's http/pg
// auto-instrumentation works by hooking Node's module loader (require-in-the-middle for CJS,
// import-in-the-middle for ESM) so that when the target module is later required/imported, OTel gets a
// chance to wrap it first. That hook has to be registered before ANYTHING else requires 'http' or
// 'pg' — including adapter-node's own server bootstrap, which creates the http.Server before
// hooks.server.ts (and everything it imports) ever runs. A bundler is free to inline, rewrite, or
// hoist module resolution in ways that defeat that hook (the exact risk flagged in the plan's §8); a
// plain, unbundled ESM file loaded via Node's own `--import` flag sidesteps the bundler entirely and
// is the pattern OTel's docs recommend for this reason.
//
// Wired via Dockerfile: `ENTRYPOINT ["node", "--import", "./instrumentation.mjs", "build/index.js"]`.
// `--import` (Node >=18.18/20) loads and awaits this module before evaluating the entrypoint, in the
// same process — the ordering guarantee this depends on.
//
// Degradation, matching packages/shared-go/obs's Go-side contract (D-b) so the dashboard fails the
// same way the Go services do:
//   - No OTEL_EXPORTER_OTLP_(TRACES_)?ENDPOINT set -> tracing is skipped entirely. No exporter is
//     constructed, no network call is ever attempted, and the process starts exactly as it would with
//     this file absent. "Absent env means no export, no crash" — a dashboard that won't boot without a
//     collector is worse than one without traces.
//   - OTEL_SDK_DISABLED=true (the standard OTel env var) -> same as above, explicitly honoured.
//   - Endpoint set but collector unreachable -> the OTLP exporter's own retry/timeout handles it
//     asynchronously; this file never awaits an export and never fails process startup because of it.

// Per-signal env var first, per the OTel spec (OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is sufficient on its
// own and takes precedence over the generic OTEL_EXPORTER_OTLP_ENDPOINT). The generic var alone used to
// be the only one checked here, which meant setting ONLY the standard per-signal var silently disabled
// tracing while this file's own comments claimed the standard vars were honoured.
const endpoint = process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT ?? process.env.OTEL_EXPORTER_OTLP_ENDPOINT;
const disabled = /^(1|true)$/i.test((process.env.OTEL_SDK_DISABLED ?? '').trim());

function logJSON(level, event, fields = {}) {
	// Same flat-JSON shape as src/lib/server/observability/log.ts (service/event/level/time), kept as a
	// literal object here rather than importing that module: this file runs before the SvelteKit
	// server (and its module graph) exists, so it has nothing to import from yet.
	const line = { time: new Date().toISOString(), level, service: 'dashboard-web', event, ...fields };
	// eslint-disable-next-line no-console -- this IS the structured-logging sink.
	console.log(JSON.stringify(line));
}

// --- Query-string redaction (security: no secret/PII span attributes) -----------------------------
//
// This app's Auth.js magic-link callback (/auth/callback/email?token=...&email=...) carries a live
// login token AND the user's email as query parameters. HttpInstrumentation records the incoming
// request's raw query string verbatim as the `url.query` span attribute — reproduced end to end: a curl
// against that URL produced a span with `url.query = "token=MAGICLINKSECRET123&email=victim%40example.com"`,
// shipped in cleartext to whatever reads the trace backend (Jaeger in this repo, D-c). This repo has
// already leaked a credential once by logging a DSN password (see docs/memory); a span is just as
// exported as a log line, so the same rule applies: never put a secret where a viewer of the trace
// backend can read it.
//
// Fix is a blanket redaction of every incoming request's query string, not an allowlist of "sensitive"
// routes: `ignoreIncomingRequestHook` could skip span creation for /auth/* entirely, but (a) that also
// blinds tracing for exactly the flows most worth debugging when login is broken, and (b) query-string
// secrets are not guaranteed to stay confined to /auth/* forever — an allowlist of "safe" paths rots the
// moment a new route adds its own token/reset-code param and nobody remembers to add it here. Redacting
// values everywhere while keeping key names costs nothing on routes with no secrets and closes the door
// on routes we haven't thought of yet.
function redactQueryString(rawQuery) {
	if (!rawQuery) return rawQuery;
	return rawQuery
		.split('&')
		.map((pair) => {
			const eq = pair.indexOf('=');
			if (eq === -1) return pair; // bare flag with no value (e.g. "debug") — nothing to redact
			const key = pair.slice(0, eq);
			return `${key}=REDACTED`;
		})
		.join('&');
}

// Exported for unit tests (tests/instrumentation-redaction.test.ts) — this file is loaded via Node's
// `--import`, never bundled, so a plain named export is safe here without affecting the runtime wiring
// below, which only uses it internally.
export { redactQueryString };

// requestHook runs once per span, AFTER HttpInstrumentation has already set its normal attributes
// (including the raw `url.query`) but before the span is exported — see
// @opentelemetry/instrumentation-http's _incomingRequestFunction, which calls the configured
// requestHook right after starting the span. Overwriting the attribute here is therefore sufficient;
// nothing downstream ever sees the raw value. Only IncomingMessage (server-side, i.e. requests INTO
// this app) carries `.url`; ClientRequest (outgoing requests this app makes) does not, and url.query
// is never set for those spans in the first place — the `typeof request.url === 'string'` guard is just
// making that existing asymmetry explicit rather than depending on it silently.
//
// Built by makeRedactQuerySpanAttribute (below, inside the try block) once ATTR_URL_QUERY is available
// from @opentelemetry/semantic-conventions, rather than a literal 'url.query' string — pinning the
// attribute name to the same semconv version as the rest of this SDK means a future semconv rename
// surfaces as an import error here instead of a silent stop-redacting.
function makeRedactQuerySpanAttribute(ATTR_URL_QUERY) {
	return function redactQuerySpanAttribute(span, request) {
		if (typeof request.url !== 'string') return;
		const qIndex = request.url.indexOf('?');
		if (qIndex === -1) return;
		span.setAttribute(ATTR_URL_QUERY, redactQueryString(request.url.slice(qIndex + 1)));
	};
}

// --- client.address / user_agent.original: DECISION = keep, on spans only -------------------------
//
// packages/shared-go/obs/provider.go strips both (plus server.address/port, network peer addr/port,
// url.path, url.full) from the Go services' METRICS via an sdkmetric View, but its own comment is
// explicit about why spans are exempt: "Spans are unaffected — a span attribute costs nothing per
// distinct value, and server.address is genuinely useful there." The hazard on the Go side is
// cardinality — an unauthenticated caller varying one header mints a new Prometheus time series per
// distinct value, an unbounded-memory-growth risk specific to a long-lived metrics registry. This
// dashboard's OTel setup (below) has no meter/metrics pipeline at all, only a trace exporter — so that
// hazard does not exist here in the first place; there is no registry for a varying attribute to grow.
// A span attribute is written once per already-finite request and shipped, not accumulated in-process.
// client.address (caller IP) and user_agent.original (browser UA) are ordinary, industry-standard
// per-request span data — the same kind of thing an access log records — and genuinely useful for
// diagnosing a specific incident (distinguishing one caller's requests from another's in a trace).
// Deliberately keeping them, not an oversight.
// ------------------------------------------------------------------------------------------------

// SHUTDOWN_FLUSH_TIMEOUT_MS bounds how long this file waits on `sdk.shutdown()` before logging and
// moving on. Measured SIGTERM->exit against a blackholed collector: 30.1s, ending in Timeout — Docker's
// default stop_grace_period is 10s, so an unbounded wait here risks SIGKILL before the "shutdown" log
// line, let alone before adapter-node's own drain (see below) gets to finish. 3s is generous for the
// batch span processor to flush its (small, dev-traffic-sized) queue to a REACHABLE collector while
// still leaving headroom under the 10s default grace period for adapter-node's own drain to complete
// afterwards.
//
// This alone is NOT sufficient, though — measured directly (a blackholed collector, one real span
// queued, SIGTERM sent): giving up on AWAITING sdk.shutdown() after 3s does not close the underlying
// OTLP HTTP request's socket. That handle is what actually keeps the Node event loop (and therefore the
// process) alive; it closes on its own once OTLPTraceExporter's OWN per-export timeout aborts the
// request — 10000ms by default, i.e. exactly Docker's default grace period, measured end to end as a
// 10.03s SIGTERM->exit. OTLP_EXPORT_TIMEOUT_MS below shortens that to the same value
// packages/shared-go/obs/provider.go uses for the identical reason (defaultOTLPTimeout, D-b: "must never
// make ... latency depend on [the collector]") — 5s leaves headroom under the grace period on both sides
// of the race, instead of relying on the SDK's own default lining up with ours by coincidence.
const SHUTDOWN_FLUSH_TIMEOUT_MS = 3000;
const OTLP_EXPORT_TIMEOUT_MS = 5000;

/** Resolves/rejects with `promise`, or rejects after `ms` if it hasn't settled — whichever comes first.
 * The timer is cleared either way (and `.unref()`'d defensively) so it can never itself be the reason
 * the process stays alive past when it would otherwise exit. */
function withTimeout(promise, ms) {
	let timer;
	const timeout = new Promise((_, reject) => {
		timer = setTimeout(() => reject(new Error(`timed out after ${ms}ms`)), ms);
		timer.unref?.();
	});
	return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
}

if (!endpoint || disabled) {
	logJSON('info', 'obs.tracing_disabled', {
		reason: disabled ? 'OTEL_SDK_DISABLED=true' : 'OTEL_EXPORTER_OTLP_(TRACES_)?ENDPOINT not set',
	});
} else {
	try {
		const { NodeSDK } = await import('@opentelemetry/sdk-node');
		const { OTLPTraceExporter } = await import('@opentelemetry/exporter-trace-otlp-http');
		const { HttpInstrumentation } = await import('@opentelemetry/instrumentation-http');
		const { PgInstrumentation } = await import('@opentelemetry/instrumentation-pg');
		const { envDetector } = await import('@opentelemetry/resources');
		const { ATTR_URL_QUERY } = await import('@opentelemetry/semantic-conventions');

		const sdk = new NodeSDK({
			serviceName: 'dashboard-web',
			// Endpoint/headers/protocol are otherwise read from the standard OTEL_EXPORTER_OTLP_* env
			// vars by the exporter itself (https://opentelemetry.io/docs/languages/sdk-configuration/otlp-exporter/)
			// — nothing dashboard-specific to configure beyond the presence check above.
			traceExporter: new OTLPTraceExporter({ timeoutMillis: OTLP_EXPORT_TIMEOUT_MS }),
			// NodeSDK's default (unset) resourceDetectors is [envDetector, processDetector, hostDetector]
			// — the same host/process metadata packages/shared-go/obs/provider.go deliberately excludes
			// on the Go side, for the same reason (buildResource's comment there): it hands the resource
			// (published on EVERY span/trace, visible to anyone with trace-backend access) the container
			// hostname, OS user, pid, and full argv — argv in particular can carry flags or credentials in
			// other deployments. Restricting to envDetector alone keeps OTEL_RESOURCE_ATTRIBUTES/
			// OTEL_SERVICE_NAME working (anything genuinely wanted can still be supplied that way) without
			// the two detectors nobody asked for.
			resourceDetectors: [envDetector],
			instrumentations: [
				new HttpInstrumentation({ requestHook: makeRedactQuerySpanAttribute(ATTR_URL_QUERY) }),
				// PgInstrumentation targets node-postgres ('pg'). This app's Drizzle setup
				// (src/lib/server/db.ts) uses the postgres-js driver ('postgres') instead, which has no
				// official OTel auto-instrumentation as of this writing. Kept per the plan's "http/pg"
				// wording and for forward-compat if a 'pg'-based path is ever added; against the
				// current driver it is an inert no-op, not a source of spans — see the report for this
				// caveat.
				new PgInstrumentation(),
			],
		});

		sdk.start();
		logJSON('info', 'obs.tracing_started', { endpoint });

		const shutdown = (signal) => {
			withTimeout(sdk.shutdown(), SHUTDOWN_FLUSH_TIMEOUT_MS)
				.then(() => logJSON('info', 'obs.tracing_shutdown', { signal }))
				.catch((err) => logJSON('warn', 'obs.tracing_shutdown_failed', { signal, error: String(err) }));
			// Deliberately NOT calling process.exit() here. adapter-node's own build/index.js installs
			// ITS OWN 'SIGTERM'/'SIGINT' listener (files/index.js's graceful_shutdown) that closes the
			// http server and drains in-flight requests, honouring SHUTDOWN_TIMEOUT (default 30s) —
			// verified by reading that file, not assumed. Node runs every listener registered for a
			// signal, so this handler and adapter-node's run concurrently off the same SIGTERM/SIGINT;
			// calling process.exit(0) the instant OUR flush settled used to race that drain and could
			// kill the process mid-request well before adapter-node was done, independent of whether the
			// flush itself was fast or slow. Once neither handler force-exits, the process ends the
			// normal Node way — when no handle keeps the event loop alive, i.e. once adapter-node's
			// server has closed AND this shutdown's own (unref'd, bounded) timer/promise has settled.
		};
		process.once('SIGTERM', () => shutdown('SIGTERM'));
		process.once('SIGINT', () => shutdown('SIGINT'));
	} catch (err) {
		// A bootstrap failure (bad local config, a missing optional dep) must degrade like everything
		// else here — the process still starts and serves requests, just without spans.
		logJSON('error', 'obs.tracing_start_failed', { error: err instanceof Error ? err.message : String(err) });
	}
}
