// Structured server-side logger. Every line is one JSON object on stdout carrying `service` and,
// whenever the call happens inside a request wrapped by hooks.server.ts's `runWithTraceContext`,
// `trace_id`/`span_id` — mirroring packages/shared-go/obs's slog.Handler (see log.go), which injects
// the same two keys onto any record logged through a context carrying a span. Call sites here never
// thread the trace id by hand, same guarantee as the Go side.
//
// Server-only (see trace.ts for why). Route handlers, $lib/server/*, and $lib/db/* should import this
// instead of $lib/logger.ts.

import { LOG_KEY_EVENT, LOG_KEY_SERVICE, LOG_KEY_SPAN_ID, LOG_KEY_TRACE_ID, SERVICE_NAME } from './constants';
import { getTraceContext } from './trace';

export type LogFields = Record<string, unknown>;

function serializeError(err: unknown): { message: string; stack?: string } {
	if (err instanceof Error) {
		return { message: err.message, stack: err.stack };
	}
	return { message: String(err) };
}

function emit(level: 'info' | 'warn' | 'error', event: string, fields?: LogFields): void {
	const ctx = getTraceContext();
	const line: Record<string, unknown> = {
		time: new Date().toISOString(),
		level,
		[LOG_KEY_SERVICE]: SERVICE_NAME,
		[LOG_KEY_EVENT]: event,
		...(ctx ? { [LOG_KEY_TRACE_ID]: ctx.traceId, [LOG_KEY_SPAN_ID]: ctx.spanId } : {}),
		...fields,
	};
	// eslint-disable-next-line no-console -- this IS the structured-logging sink (stdout, one JSON line).
	console.log(JSON.stringify(line));
}

export const log = {
	info(event: string, fields?: LogFields): void {
		emit('info', event, fields);
	},
	warn(event: string, fields?: LogFields): void {
		emit('warn', event, fields);
	},
	/** `error` field, if present, is passed through `serializeError` (Error -> {message, stack}, else
	 * String(err)) rather than logged raw — an Error object stringifies to `{}` under JSON.stringify. */
	error(event: string, fields?: LogFields & { error?: unknown }): void {
		const { error: err, ...rest } = fields ?? {};
		emit('error', event, err !== undefined ? { ...rest, error: serializeError(err) } : rest);
	},
};
