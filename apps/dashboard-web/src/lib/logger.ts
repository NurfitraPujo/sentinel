// Isomorphic structured logger — safe to import from BOTH client and server code (no Node built-ins).
// Server-side code (routes, $lib/server/*, $lib/db/*) should prefer
// `$lib/server/observability/log.ts` instead: it wraps this same JSON shape but also injects the
// current request's trace_id/span_id from AsyncLocalStorage automatically. This module exists so the
// handful of client-only call sites (e.g. a `<script>` block in a .svelte component) can still emit a
// structured line instead of a bare `console.log(...)` — see OBSERVABILITY_PLAN.md D-f ("the ~12
// console.* calls in src/ become structured JSON lines including service: dashboard-web").
//
// No trace id is attached here: a browser-side click handler has no server request/span in scope, and
// that is fine — the plan only requires the trace id "when available".

export type LogFields = Record<string, unknown>;

const SERVICE_NAME = 'dashboard-web';

function serializeError(err: unknown): { message: string; stack?: string } | undefined {
	if (err === undefined) return undefined;
	if (err instanceof Error) {
		return { message: err.message, stack: err.stack };
	}
	return { message: String(err) };
}

function emit(level: 'info' | 'warn' | 'error', event: string, fields?: LogFields): void {
	const line: Record<string, unknown> = {
		time: new Date().toISOString(),
		level,
		service: SERVICE_NAME,
		event,
		...fields,
	};
	// eslint-disable-next-line no-console -- this IS the structured-logging sink.
	console.log(JSON.stringify(line));
}

export const logger = {
	info(event: string, fields?: LogFields): void {
		emit('info', event, fields);
	},
	warn(event: string, fields?: LogFields): void {
		emit('warn', event, fields);
	},
	error(event: string, fields?: LogFields & { error?: unknown }): void {
		const { error: err, ...rest } = fields ?? {};
		emit('error', event, err !== undefined ? { ...rest, error: serializeError(err) } : rest);
	},
};
